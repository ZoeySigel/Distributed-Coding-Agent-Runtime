//go:build linux

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run in an isolated disposable test container, not on an arbitrary host root.
func TestCollectIgnoresAgentGitHistory(t *testing.T) {
	if os.Getenv("WORKSPACE_INTEGRATION") != "1" {
		t.Skip("requires disposable container with /workspace /baseline /source")
	}
	ctx := context.Background()
	for _, p := range []string{"/workspace/repo", "/baseline", "/source"} {
		if e := os.MkdirAll(p, 0755); e != nil {
			t.Fatal(e)
		}
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("/workspace/repo", "init")
	os.WriteFile("/workspace/repo/old.txt", []byte("old\n"), 0644)
	git("/workspace/repo", "add", ".")
	git("/workspace/repo", "commit", "-m", "base")
	sha := git("/workspace/repo", "rev-parse", "HEAD")
	git("/baseline", "clone", "--bare", "--no-hardlinks", "/workspace/repo", "repo.git")
	os.Remove("/workspace/repo/old.txt")
	os.WriteFile("/workspace/repo/new.txt", []byte("new\n"), 0755)
	os.WriteFile("/workspace/repo/binary.bin", []byte{0, 1, 2, 3}, 0644)
	git("/workspace/repo", "add", "-A")
	git("/workspace/repo", "commit", "-m", "agent commit")
	git("/source", "clone", "--no-hardlinks", "/workspace/repo", "repo")
	got, e := Collect(ctx, Input{SHA: sha})
	if e != nil {
		t.Fatal(e)
	}
	for _, text := range []string{"new.txt", "old.txt", "binary.bin", "GIT binary patch", "100755"} {
		if !strings.Contains(string(got.Patch), text) {
			t.Fatalf("missing %s in %s", text, got.Patch)
		}
	}
	if len(got.Files) != 3 {
		t.Fatal(got.Files)
	}
}

func TestResolvedSHASurvivesBranchUpdates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git: %s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-b", "main")
	if e := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0644); e != nil {
		t.Fatal(e)
	}
	git("add", ".")
	git("commit", "-m", "base")
	old, e := Resolve(ctx, Input{Repository: dir, Ref: "refs/heads/main"})
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("updated"), 0644); e != nil {
		t.Fatal(e)
	}
	git("add", ".")
	git("commit", "-m", "update")
	next, e := Resolve(ctx, Input{Repository: dir, Ref: "refs/heads/main"})
	if e != nil || next.SHA == old.SHA {
		t.Fatal("branch did not advance", e)
	}
	retry, e := Resolve(ctx, Input{Repository: dir, Ref: "refs/heads/main", SHA: old.SHA})
	if e != nil || retry.SHA != old.SHA {
		t.Fatal("retry resolved changed branch", e)
	}
}
