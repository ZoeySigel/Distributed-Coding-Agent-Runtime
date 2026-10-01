package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dcar/runtime/internal/domain"
)

// ClientSettings stores connection metadata, never a model key or copied token.
type ClientSettings struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

func ClientConfigPath() (string, error) {
	if p := os.Getenv("DCAR_CONFIG"); p != "" {
		return filepath.Abs(p)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dcar", "client.json"), nil
}
func LoadClientSettings() (ClientSettings, error) {
	s := ClientSettings{URL: "http://localhost:8080", TokenFile: "secrets/api-tokens.json"}
	path, err := ClientConfigPath()
	if err != nil {
		return s, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("invalid client config: %w", err)
	}
	if s.TokenFile != "" && !filepath.IsAbs(s.TokenFile) {
		s.TokenFile = filepath.Join(filepath.Dir(path), s.TokenFile)
	}
	return s, nil
}
func SaveClientSettings(s ClientSettings) (string, error) {
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("API URL must be HTTP(S), without credentials, query or fragment")
	}
	s.URL = strings.TrimRight(s.URL, "/")
	s.TokenFile, err = filepath.Abs(s.TokenFile)
	if err != nil {
		return "", err
	}
	if _, err = ReadClientToken(s.TokenFile); err != nil {
		return "", err
	}
	path, err := ClientConfigPath()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".client-*.json")
	if err != nil {
		return "", err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(temporary, path); err != nil {
		return "", err
	}
	return path, nil
}
func ReadClientToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var tokens map[string]string
	if err = json.Unmarshal(b, &tokens); err != nil {
		return "", fmt.Errorf("invalid API token JSON: %w", err)
	}
	if tokens["local"] == "" {
		return "", fmt.Errorf("API token JSON has no local entry")
	}
	return tokens["local"], nil
}

// GitHubWorkingRepository only reads local Git metadata; it does not fetch or push.
func GitHubWorkingRepository(ctx context.Context) (repository, sha, note string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	run := func(args ...string) string {
		b, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	remote := run("remote", "get-url", "origin")
	if strings.HasPrefix(remote, "git@github.com:") {
		remote = "https://github.com/" + strings.TrimPrefix(remote, "git@github.com:")
	} else if strings.HasPrefix(remote, "ssh://git@github.com/") {
		remote = "https://github.com/" + strings.TrimPrefix(remote, "ssh://git@github.com/")
	}
	s := domain.Spec{Repository: remote, Prompt: "detect"}
	if s.Normalize() != nil {
		return "", "", "No GitHub origin detected; enter a repository when creating a task."
	}
	repository = s.Repository
	sha = run("rev-parse", "--verify", "HEAD")
	if len(sha) != 40 && len(sha) != 64 {
		sha = ""
	}
	note = "Detected GitHub origin; execution clones the selected commit remotely."
	if run("status", "--porcelain", "--untracked-files=normal") != "" {
		note = "Local changes detected; commit and push before submission. Local files are not uploaded."
	}
	return
}
