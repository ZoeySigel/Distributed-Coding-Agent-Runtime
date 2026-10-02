package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DetectPlan runs before dependency installation and before the editing agent.
// A build-only plan is explicitly labelled and never reported as functional tests.
func DetectPlan(root, sha, prepareOverride, testOverride string) (domain.ExecutionPlan, error) {
	p := domain.ExecutionPlan{SHA: sha, Source: "detected", Kind: "unavailable", Rationale: "No supported verification configuration was found."}
	exists := func(name string) bool {
		info, e := os.Stat(filepath.Join(root, name))
		return e == nil && !info.IsDir()
	}
	systems := 0
	if exists("go.mod") || exists("go.work") {
		systems++
	}
	if exists("package.json") {
		systems++
	}
	py := exists("pytest.ini")
	for _, file := range []string{"pyproject.toml", "setup.cfg", "tox.ini"} {
		b, _ := os.ReadFile(filepath.Join(root, file))
		py = py || bytes.Contains(b, []byte("[tool.pytest.ini_options]")) || bytes.Contains(b, []byte("[tool:pytest]")) || bytes.Contains(b, []byte("[pytest]"))
	}
	if py {
		systems++
	}
	if systems > 1 || exists("go.work") {
		p.Rationale = "Multiple project systems or a Go workspace need repository-specific planning."
	} else if exists("go.mod") {
		p.PrepareCommand = "go mod download"
		p.TestCommand = "go test ./..."
		p.Kind = "tests"
		p.Rationale = "Root Go module: download declared dependencies and run all module tests."
	} else if exists("package.json") {
		b, e := os.ReadFile(filepath.Join(root, "package.json"))
		if e != nil {
			return p, e
		}
		var pkg struct {
			Scripts        map[string]string `json:"scripts"`
			PackageManager string            `json:"packageManager"`
		}
		if e = json.Unmarshal(b, &pkg); e != nil {
			return p, fmt.Errorf("invalid package.json: %w", e)
		}
		manager := "npm"
		switch {
		case strings.HasPrefix(pkg.PackageManager, "pnpm@") || exists("pnpm-lock.yaml"):
			manager = "corepack pnpm"
			p.PrepareCommand = manager + " install --frozen-lockfile"
		case strings.HasPrefix(pkg.PackageManager, "yarn@") || exists("yarn.lock"):
			manager = "corepack yarn"
			flag := "--frozen-lockfile"
			if pkg.PackageManager != "" && !strings.HasPrefix(pkg.PackageManager, "yarn@1.") {
				flag = "--immutable"
			}
			p.PrepareCommand = manager + " install " + flag
		default:
			p.PrepareCommand = "npm install --no-audit --no-fund"
			if exists("package-lock.json") || exists("npm-shrinkwrap.json") {
				p.PrepareCommand = "npm ci --no-audit --no-fund"
			}
		}
		switch {
		case strings.TrimSpace(pkg.Scripts["test"]) != "" && !strings.Contains(pkg.Scripts["test"], "no test specified"):
			p.TestCommand = manager + " test"
			p.Kind = "tests"
		case strings.TrimSpace(pkg.Scripts["build"]) != "":
			p.TestCommand = manager + " run build"
			p.Kind = "build"
		case strings.TrimSpace(pkg.Scripts["lint"]) != "":
			p.TestCommand = manager + " run lint"
			p.Kind = "lint"
		}
		p.Rationale = "Use the original package manager and lockfile; prefer tests, then a declared build or lint check. Build/lint checks do not prove functional correctness."
	} else if py {
		p.TestCommand = "python3 -m pytest"
		p.Kind = "tests"
		p.Rationale = "Original pytest configuration; use the workspace pytest toolchain."
		metadata, _ := os.ReadFile(filepath.Join(root, "pyproject.toml"))
		projectPackage := bytes.Contains(metadata, []byte("[project]")) || bytes.Contains(metadata, []byte("[build-system]"))
		if exists("requirements.txt") || projectPackage {
			p.PrepareCommand = "python3 -m venv /home/agent/project-venv && /home/agent/project-venv/bin/pip install pytest==8.3.5"
			if exists("requirements.txt") {
				p.PrepareCommand += " && /home/agent/project-venv/bin/pip install -r requirements.txt"
			} else {
				p.PrepareCommand += " && /home/agent/project-venv/bin/pip install -e ."
			}
			p.TestCommand = "/home/agent/project-venv/bin/python -m pytest"
		}
	}
	if prepareOverride != "" {
		p.PrepareCommand = prepareOverride
	}
	if testOverride != "" {
		p.TestCommand = testOverride
		p.Kind = "custom"
	}
	if prepareOverride != "" || testOverride != "" {
		p.Source = "explicit"
		p.Rationale += " Submitter commands override corresponding detected commands."
	}
	return p, p.Validate()
}

const PlannerPrompt = `Inspect the ORIGINAL repository mounted read-only at /workspace/repo. Do not edit files, install dependencies, or run project code. Read package manifests, CI, Makefiles, README and existing tests. Propose bounded POSIX shell commands for the available toolchain. Do not invent dependencies, scripts or a passing test. Prefer existing tests; otherwise select an existing build/lint/static check and label its coverage honestly. Never use echo, true, exit 0, or git diff as verification. If no meaningful check exists, return kind unavailable with an empty test_command and explain why. Commands will be executed by the platform under its timeout and egress controls. Return ONLY a JSON object with these four fields: prepare_command (string, may be empty), test_command (string), kind (tests|build|lint|static|unavailable), rationale (string). Do not include Markdown or any other fields.`

func ParseProposal(raw []byte, sha string) (domain.ExecutionPlan, error) {
	var proposal struct {
		Prepare   string `json:"prepare_command"`
		Test      string `json:"test_command"`
		Kind      string `json:"kind"`
		Rationale string `json:"rationale"`
	}
	p := domain.ExecutionPlan{SHA: sha, Source: "agent"}
	if len(raw) > 16384 {
		return p, fmt.Errorf("planner output exceeds 16 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&proposal); e != nil {
		return p, fmt.Errorf("invalid planner JSON: %w", e)
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return p, fmt.Errorf("planner must return exactly one JSON object")
	}
	p.PrepareCommand = proposal.Prepare
	p.TestCommand = proposal.Test
	p.Kind = proposal.Kind
	p.Rationale = proposal.Rationale
	if p.Kind == "custom" {
		return p, fmt.Errorf("planner must identify check coverage")
	}
	return p, p.Validate()
}
