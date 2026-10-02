package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutomaticPreparationAndCoverage(t *testing.T) {
	for _, tc := range []struct {
		name                string
		files               map[string]string
		prepare, test, kind string
	}{
		{"go", map[string]string{"go.mod": "module example"}, "go mod download", "go test ./...", "tests"},
		{"vue-build", map[string]string{"package.json": `{"scripts":{"build":"vue-cli-service build","lint":"vue-cli-service lint"}}`, "package-lock.json": "{}"}, "npm ci --no-audit --no-fund", "npm run build", "build"},
		{"node-tests", map[string]string{"package.json": `{"scripts":{"test":"node --test","build":"node build.js"}}`}, "npm install --no-audit --no-fund", "npm test", "tests"},
		{"placeholder", map[string]string{"package.json": `{"scripts":{"test":"echo no test specified && exit 1","build":"webpack"}}`}, "npm install --no-audit --no-fund", "npm run build", "build"},
		{"pnpm", map[string]string{"package.json": `{"packageManager":"pnpm@9.15.0","scripts":{"test":"vitest run"}}`, "pnpm-lock.yaml": "lockfileVersion: 9"}, "corepack pnpm install --frozen-lockfile", "corepack pnpm test", "tests"},
		{"monorepo-root-check", map[string]string{"package.json": `{"workspaces":["packages/*"],"scripts":{"test":"npm test --workspaces"}}`}, "npm install --no-audit --no-fund", "npm test", "tests"},
		{"pytest-only-config", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]"}, "", "python3 -m pytest", "tests"},
		{"unknown", map[string]string{"README.md": "custom toolchain"}, "", "", "unavailable"},
		{"ambiguous", map[string]string{"go.mod": "module a", "package.json": `{"scripts":{"test":"node --test"}}`}, "", "", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for f, b := range tc.files {
				if e := os.WriteFile(filepath.Join(root, f), []byte(b), 0600); e != nil {
					t.Fatal(e)
				}
			}
			p, e := DetectPlan(root, strings.Repeat("a", 40), "", "")
			if e != nil || p.PrepareCommand != tc.prepare || p.TestCommand != tc.test || p.Kind != tc.kind {
				t.Fatalf("%+v %v", p, e)
			}
			override, e := DetectPlan(root, p.SHA, "setup", "verify")
			if e != nil || override.Source != "explicit" || override.PrepareCommand != "setup" || override.TestCommand != "verify" {
				t.Fatal(override, e)
			}
		})
	}
}

func TestPlannerProposalValidation(t *testing.T) {
	sha := strings.Repeat("a", 40)
	valid := `{"prepare_command":"","test_command":"make check","kind":"tests","rationale":"Original Makefile provides a check target."}`
	p, e := ParseProposal([]byte(valid), sha)
	if e != nil || p.Source != "agent" || p.TestCommand != "make check" {
		t.Fatal(p, e)
	}
	for _, raw := range []string{
		valid + valid,
		"```json\n" + valid + "\n```",
		strings.Replace(valid, `"make check"`, `"true"`, 1),
		strings.Replace(valid, `"tests"`, `"custom"`, 1),
		strings.Replace(valid, `"make check"`, `""`, 1),
		strings.Replace(valid, `"prepare_command"`, `"ignored_command"`, 1),
		strings.Repeat("x", 16385),
	} {
		if _, e := ParseProposal([]byte(raw), sha); e == nil {
			t.Errorf("accepted invalid proposal: %.80s", raw)
		}
	}
	if _, e := ParseProposal([]byte(valid), "branch"); e == nil {
		t.Fatal("accepted mutable baseline")
	}
}
