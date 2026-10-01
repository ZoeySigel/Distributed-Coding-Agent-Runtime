package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Marker = "__DCAR_RESULT__"
const OutputLimit = 1 << 20

type Input struct {
	Repository   string `json:"repository"`
	Ref          string `json:"ref"`
	SHA          string `json:"sha"`
	Credential   string `json:"credential"`
	Command      string `json:"command"`
	Seconds      int    `json:"seconds"`
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
	ModelCatalog string `json:"model_catalog,omitempty"`
	Executor     string `json:"executor"`
	TestCommand  string `json:"test_command"`
}
type Prepared struct {
	SHA               string `json:"sha"`
	TestCommand       string `json:"test_command"`
	VerificationError string `json:"verification_error,omitempty"`
}
type Collected struct {
	Patch         []byte   `json:"patch"`
	Files         []string `json:"files"`
	TestsModified []string `json:"tests_modified"`
	SHA           string   `json:"sha"`
}
type AgentResult struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	Completed bool   `json:"completed"`
}
type Limited struct {
	Buffer    bytes.Buffer
	Limit     int
	Truncated bool
}

func (l *Limited) Write(p []byte) (int, error) {
	n := len(p)
	left := max(0, l.Limit-l.Buffer.Len())
	if len(p) > left {
		l.Truncated = true
		p = p[:left]
	}
	_, _ = l.Buffer.Write(p)
	return n, nil
}
func Emit(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	fmt.Println(Marker + base64.StdEncoding.EncodeToString(b))
	return nil
}
func Parse(b []byte, out any) error {
	i := bytes.LastIndex(b, []byte(Marker))
	if i < 0 {
		return fmt.Errorf("runner did not emit a result")
	}
	line := strings.TrimSpace(string(b[i+len(Marker):]))
	data, e := base64.StdEncoding.DecodeString(line)
	if e != nil {
		return e
	}
	return json.Unmarshal(data, out)
}
func ReadInput(path string) (Input, error) {
	var v Input
	b, e := os.ReadFile(path)
	if e != nil {
		return v, e
	}
	_ = os.Remove(path)
	e = json.Unmarshal(b, &v)
	return v, e
}
func run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out := &Limited{Limit: OutputLimit}
	cmd.Stdout = out
	cmd.Stderr = out
	e := cmd.Run()
	if out.Truncated {
		return out.Buffer.Bytes(), fmt.Errorf("%s output exceeded 1 MiB limit", args[0])
	}
	if e != nil {
		return out.Buffer.Bytes(), fmt.Errorf("%s failed: %w: %s", args[0], e, out.Buffer.String())
	}
	return out.Buffer.Bytes(), nil
}
func Detect(root, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	exists := func(p string) bool { _, e := os.Stat(filepath.Join(root, p)); return e == nil }
	var candidates []string
	if exists("go.work") {
		return "", fmt.Errorf("Go workspace requires explicit test_command")
	}
	if exists("go.mod") {
		candidates = append(candidates, "go test ./...")
	}
	if exists("package.json") {
		b, e := os.ReadFile(filepath.Join(root, "package.json"))
		if e != nil {
			return "", e
		}
		var p struct {
			Scripts    map[string]string `json:"scripts"`
			Workspaces json.RawMessage   `json:"workspaces"`
		}
		if json.Unmarshal(b, &p) != nil {
			return "", fmt.Errorf("invalid package.json")
		}
		if len(p.Workspaces) > 0 {
			return "", fmt.Errorf("Node workspace requires explicit test_command")
		}
		if p.Scripts["test"] != "" {
			candidates = append(candidates, "npm test")
		}
	}
	py := exists("pytest.ini")
	for _, f := range []string{"pyproject.toml", "setup.cfg", "tox.ini"} {
		if b, e := os.ReadFile(filepath.Join(root, f)); e == nil && (bytes.Contains(b, []byte("[tool.pytest.ini_options]")) || bytes.Contains(b, []byte("[tool:pytest]")) || bytes.Contains(b, []byte("[pytest]"))) {
			py = true
		}
	}
	if py {
		candidates = append(candidates, "python3 -m pytest")
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("expected one test system, found %d; supply test_command", len(candidates))
	}
	return candidates[0], nil
}
func Prepare(ctx context.Context, in Input) (Prepared, error) {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if in.Credential != "" {
		if e := os.WriteFile("/tmp/askpass", []byte("#!/bin/sh\ncase \"$1\" in *Username*) printf '%s' 'x-access-token';; *) printf '%s' \"$DCAR_GIT_TOKEN\";; esac\n"), 0700); e != nil {
			return Prepared{}, e
		}
		defer os.Remove("/tmp/askpass")
		env = append(env, "GIT_ASKPASS=/tmp/askpass", "DCAR_GIT_TOKEN="+in.Credential)
	}
	cleanErr := func(e error) error {
		if e == nil {
			return nil
		}
		s := e.Error()
		if in.Credential != "" {
			s = strings.ReplaceAll(s, in.Credential, "[REDACTED]")
		}
		return fmt.Errorf("%s", s)
	}
	if _, e := run(ctx, "/workspace", env, "git", "-c", "core.hooksPath=/dev/null", "clone", "--no-checkout", "--no-recurse-submodules", "--", in.Repository, "/workspace/repo"); e != nil {
		return Prepared{}, cleanErr(e)
	}
	ref := in.SHA
	if ref == "" {
		ref = in.Ref
	}
	if ref == "" {
		ref = "HEAD"
	} else {
		if _, e := run(ctx, "/workspace/repo", env, "git", "fetch", "--no-recurse-submodules", "origin", ref); e != nil {
			return Prepared{}, cleanErr(e)
		}
		ref = "FETCH_HEAD"
	}
	b, e := run(ctx, "/workspace/repo", env, "git", "rev-parse", "--verify", ref+"^{commit}")
	if e != nil {
		return Prepared{}, cleanErr(e)
	}
	sha := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(sha) {
		return Prepared{}, fmt.Errorf("invalid resolved SHA")
	}
	if in.SHA != "" && in.SHA != sha {
		return Prepared{}, fmt.Errorf("fixed SHA mismatch")
	}
	if _, e = run(ctx, "/workspace/repo", env, "git", "-c", "core.hooksPath=/dev/null", "checkout", "--detach", sha); e != nil {
		return Prepared{}, cleanErr(e)
	}
	if _, e = os.Stat("/workspace/repo/.gitmodules"); e == nil {
		return Prepared{}, fmt.Errorf("submodules are unsupported")
	}
	b, e = run(ctx, "/workspace/repo", env, "git", "ls-files", "--stage")
	if e != nil {
		return Prepared{}, e
	}
	if bytes.Contains(b, []byte("160000 ")) {
		return Prepared{}, fmt.Errorf("submodule gitlinks are unsupported")
	}
	e = filepath.WalkDir("/workspace/repo", func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			f, e := os.Open(path)
			if e != nil {
				return e
			}
			head := make([]byte, 128)
			n, _ := f.Read(head)
			f.Close()
			if bytes.HasPrefix(head[:n], []byte("version https://git-lfs.github.com/spec/")) {
				return fmt.Errorf("Git LFS is unsupported")
			}
		}
		return nil
	})
	if e != nil {
		return Prepared{}, e
	}
	if _, e = run(ctx, "/workspace", []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}, "git", "clone", "--bare", "--no-hardlinks", "/workspace/repo", "/baseline/repo.git"); e != nil {
		return Prepared{}, e
	}
	test, ve := Detect("/workspace/repo", in.TestCommand)
	p := Prepared{SHA: sha, TestCommand: test}
	if ve != nil {
		p.VerificationError = ve.Error()
	}
	return p, nil
}
func Collect(ctx context.Context, in Input) (Collected, error) {
	return CollectPaths(ctx, in, "/source/repo", "/baseline/repo.git", "/tmp")
}
func CollectPaths(ctx context.Context, in Input, source, baseline, temp string) (Collected, error) {
	info, e := os.Lstat(source)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Collected{}, fmt.Errorf("repository root must be a real directory")
	}
	// Baseline is mounted read-only and never exposed to the agent. Ignore its HEAD/index/config entirely.
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(in.SHA) {
		return Collected{}, fmt.Errorf("invalid baseline")
	}
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	gitDir := filepath.Join(temp, "result.git")
	if _, e := run(ctx, temp, env, "git", "clone", "--bare", "--no-hardlinks", baseline, gitDir); e != nil {
		return Collected{}, e
	}
	// Git's safe.directory is explicit; collector never executes workspace hooks, filters or config.
	args := []string{"git", "--git-dir=" + gitDir, "--work-tree=" + source, "-c", "safe.directory=*", "-c", "core.bare=false", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
	call := func(extra ...string) ([]byte, error) {
		return run(ctx, source, env, append(append([]string{}, args...), extra...)...)
	}
	if _, e := call("read-tree", in.SHA); e != nil {
		return Collected{}, e
	}
	if _, e := call("add", "-A", "--", "."); e != nil {
		return Collected{}, e
	}
	names, e := call("diff", "--cached", "--name-only", "-z", in.SHA, "--")
	if e != nil {
		return Collected{}, e
	}
	files := strings.Split(strings.TrimSuffix(string(names), "\x00"), "\x00")
	if len(files) == 1 && files[0] == "" {
		files = []string{}
	}
	tests := []string{}
	for _, f := range files {
		if !safeRelativePath(f) {
			return Collected{}, fmt.Errorf("unsupported artifact path %q", f)
		}
		lower := strings.ToLower(f)
		if strings.Contains(lower, "test") || strings.Contains(lower, "spec") || strings.HasSuffix(lower, "package.json") || strings.HasSuffix(lower, "pyproject.toml") {
			tests = append(tests, f)
		}
	}
	cmd := exec.CommandContext(ctx, args[0], append(args[1:], "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", in.SHA, "--")...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = temp
	out := &Limited{Limit: 16 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if e := cmd.Run(); e != nil {
		return Collected{}, e
	}
	if out.Truncated {
		return Collected{}, fmt.Errorf("patch exceeds 16 MiB limit")
	}
	return Collected{Patch: out.Buffer.Bytes(), Files: files, TestsModified: tests, SHA: in.SHA}, nil
}
func Fixture(ctx context.Context, in Input) (AgentResult, error) {
	return FixturePaths(ctx, in, "/workspace/repo")
}
func safeRelativePath(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.ContainsAny(p, "\\:\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
func FixturePaths(ctx context.Context, in Input, root string) (AgentResult, error) {
	var v struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		DelayMS int    `json:"delay_ms"`
		Fail    bool   `json:"fail"`
	}
	if e := json.Unmarshal([]byte(in.Prompt), &v); e != nil {
		return AgentResult{}, fmt.Errorf("fixture prompt must be JSON")
	}
	if v.DelayMS > 0 {
		select {
		case <-ctx.Done():
			return AgentResult{}, ctx.Err()
		case <-time.After(time.Duration(v.DelayMS) * time.Millisecond):
		}
	}
	if v.Fail {
		return AgentResult{ExitCode: 1, Output: "fixture failure"}, nil
	}
	p := v.Path
	if !safeRelativePath(p) {
		return AgentResult{}, fmt.Errorf("invalid fixture path")
	}
	dir, e := os.OpenRoot(root)
	if e != nil {
		return AgentResult{}, e
	}
	defer dir.Close()
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		if e := dir.Mkdir(strings.Join(parts[:i], "/"), 0755); e != nil && !os.IsExist(e) {
			return AgentResult{}, e
		}
	}
	f, e := dir.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if e != nil {
		return AgentResult{}, e
	}
	_, e = f.Write([]byte(v.Content))
	closeErr := f.Close()
	if e != nil {
		return AgentResult{}, e
	}
	if closeErr != nil {
		return AgentResult{}, closeErr
	}
	return AgentResult{ExitCode: 0, Completed: true, Output: `{"type":"turn.completed"}`}, nil
}
