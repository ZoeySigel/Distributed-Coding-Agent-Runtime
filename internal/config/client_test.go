package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientConfigurationSurvivesWorkingDirectoryChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings", "client.json")
	t.Setenv("DCAR_CONFIG", path)
	tokenPath := filepath.Join(root, "token.json")
	if err := os.WriteFile(tokenPath, []byte(`{"local":"private-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	saved, err := SaveClientSettings(ClientSettings{URL: "http://localhost:18180/", TokenFile: tokenPath})
	if err != nil || saved != path {
		t.Fatalf("%s %v", saved, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "private-token") {
		t.Fatal("configuration copied token value")
	}
	// Replacing an existing config also works on Windows.
	if _, err = SaveClientSettings(ClientSettings{URL: "https://runtime.example.com", TokenFile: tokenPath}); err != nil {
		t.Fatal(err)
	}
	previous, _ := os.Getwd()
	if err = os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)
	s, err := LoadClientSettings()
	if err != nil || s.URL != "https://runtime.example.com" || s.TokenFile != tokenPath {
		t.Fatalf("%+v %v", s, err)
	}
	token, err := ReadClientToken(s.TokenFile)
	if err != nil || token != "private-token" {
		t.Fatal("credential reference did not resolve")
	}
	if _, err = SaveClientSettings(ClientSettings{URL: "https://secret:password@example.com", TokenFile: tokenPath}); err == nil {
		t.Fatal("accepted embedded credentials")
	}
	s, _ = LoadClientSettings()
	if s.URL != "https://runtime.example.com" {
		t.Fatal("invalid config replaced good config")
	}
}

func TestWorkingDirectoryDetectionHandlesGitHubRemotesAndDirtyFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git unavailable")
	}
	previous, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)
	git := func(args ...string) string {
		t.Helper()
		b, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git failed: %s", b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "baseline")
	head := git("rev-parse", "HEAD")
	git("remote", "add", "origin", "git@github.com:owner/project.git")
	for _, remote := range []string{"git@github.com:owner/project.git", "ssh://git@github.com/owner/project.git", "https://github.com/owner/project"} {
		git("remote", "set-url", "origin", remote)
		repo, sha, _ := GitHubWorkingRepository(context.Background())
		if repo != "https://github.com/owner/project.git" || sha != head {
			t.Fatalf("unexpected detected repo: %s %s", repo, sha)
		}
	}
	if err := os.WriteFile("uncommitted.txt", []byte("local only"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, note := GitHubWorkingRepository(context.Background())
	if !strings.Contains(note, "Local changes") {
		t.Fatal("missing local change notice")
	}
	git("remote", "set-url", "origin", "https://evil.example.com/owner/project.git")
	repo, sha, _ := GitHubWorkingRepository(context.Background())
	if repo != "" || sha != "" {
		t.Fatal("accepted unsupported origin")
	}
}
