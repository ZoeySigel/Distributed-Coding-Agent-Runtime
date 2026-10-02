package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchIncludesCommittedChangesAndIgnoresMaliciousConfig(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git required")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	baseline := filepath.Join(root, "base.git")
	temp := filepath.Join(root, "collect")
	for _, p := range []string{source, temp} {
		if e := os.MkdirAll(p, 0755); e != nil {
			t.Fatal(e)
		}
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	write := func(name string, b []byte) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(source, name), b, 0644); e != nil {
			t.Fatal(e)
		}
	}
	git(source, "init")
	write("old.txt", []byte("before\n"))
	git(source, "add", ".")
	git(source, "commit", "-m", "base")
	sha := git(source, "rev-parse", "HEAD")
	git(root, "clone", "--bare", "--no-hardlinks", source, baseline)
	os.Remove(filepath.Join(source, "old.txt"))
	write("new.txt", []byte("after\n"))
	write("binary.bin", []byte{0, 1, 2, 3})
	git(source, "add", "-A")
	git(source, "commit", "-m", "agent commit")
	git(source, "config", "diff.external", "nonexistent-should-never-run")
	git(source, "config", "core.hooksPath", "malicious-hooks")
	result, e := CollectPaths(context.Background(), Input{SHA: sha, CollectPublication: true}, source, baseline, temp)
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"new.txt", "old.txt", "GIT binary patch", "+after"} {
		if !strings.Contains(string(result.Patch), s) {
			t.Errorf("patch missing %s: %s", s, result.Patch)
		}
	}
	if len(result.Files) != 3 {
		t.Fatalf("files: %v", result.Files)
	}
	if e := result.ChangeSet.Validate(); e != nil {
		t.Fatal(e)
	}
	if len(result.ChangeSet.Changes) != 3 {
		t.Fatal("publication changes missing")
	}
	for _, c := range result.ChangeSet.Changes {
		switch c.Path {
		case "binary.bin":
			if string(c.Content) != string([]byte{0, 1, 2, 3}) || c.Mode != "100644" {
				t.Fatal(c)
			}
		case "old.txt":
			if !c.Delete {
				t.Fatal("deletion lost")
			}
		case "new.txt":
			if string(c.Content) != "after\n" {
				t.Fatal(c)
			}
		}
	}
}
