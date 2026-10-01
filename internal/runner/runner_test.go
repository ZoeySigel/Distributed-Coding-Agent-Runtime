package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDetection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
		err   bool
	}{{"go", map[string]string{"go.mod": "module test"}, "go test ./...", false}, {"node", map[string]string{"package.json": `{"scripts":{"test":"node --test"}}`}, "npm test", false}, {"python", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]"}, "python3 -m pytest", false}, {"none", nil, "", true}, {"ambiguous", map[string]string{"go.mod": "module test", "pytest.ini": "[pytest]"}, "", true}, {"workspace", map[string]string{"package.json": `{"workspaces":["packages/*"],"scripts":{"test":"test"}}`}, "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for f, v := range tc.files {
				if e := os.WriteFile(filepath.Join(dir, f), []byte(v), 0600); e != nil {
					t.Fatal(e)
				}
			}
			got, e := Detect(dir, "")
			if got != tc.want || (e != nil) != tc.err {
				t.Fatalf("%q %v", got, e)
			}
			got, e = Detect(dir, "custom test")
			if e != nil || got != "custom test" {
				t.Fatal("override lost")
			}
		})
	}
}

func TestFixturePathBoundaries(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escape", "a/../escape", "/absolute", "a/.git/config", `a\escape`, "C:/escape"} {
		prompt, _ := json.Marshal(map[string]string{"path": path, "content": "secret"})
		if _, e := FixturePaths(context.Background(), Input{Prompt: string(prompt)}, root); e == nil {
			t.Errorf("accepted %q", path)
		}
	}
	prompt, _ := json.Marshal(map[string]string{"path": ".github/test.txt", "content": "safe"})
	if _, e := FixturePaths(context.Background(), Input{Prompt: string(prompt)}, root); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "escape")); e != nil {
		t.Log("symlink creation unavailable on this OS")
		return
	}
	prompt, _ = json.Marshal(map[string]string{"path": "escape/outside.txt", "content": "secret"})
	if _, e := FixturePaths(context.Background(), Input{Prompt: string(prompt)}, root); e == nil {
		t.Fatal("symlink escaped root")
	}
	if _, e := os.Stat(filepath.Join(outside, "outside.txt")); !os.IsNotExist(e) {
		t.Fatal("wrote outside repository")
	}
}
func TestLimitedAndResultParsing(t *testing.T) {
	w := &Limited{Limit: 3}
	n, e := w.Write([]byte("abcdef"))
	if e != nil || n != 6 || w.Buffer.String() != "abc" || !w.Truncated {
		t.Fatal("bad writer")
	}
	b, _ := json.Marshal(Prepared{SHA: "abc"})
	raw := append([]byte("noise\n"+Marker+"invalid\n"+Marker), []byte(base64.StdEncoding.EncodeToString(b))...)
	var got Prepared
	if e := Parse(raw, &got); e != nil || got.SHA != "abc" {
		t.Fatal(got, e)
	}
	if Parse(bytes.Repeat([]byte("x"), 100), &got) == nil {
		t.Fatal("accepted missing result")
	}
}

func TestCompletionSurvivesLogTruncation(t *testing.T) {
	s := &AgentStream{Output: &Limited{Limit: 3}}
	_, _ = s.Write([]byte("log noise\n{\"type\":\"turn.com"))
	_, _ = s.Write([]byte("pleted\"}\n"))
	if !s.Completed || !s.Output.Truncated {
		t.Fatal("lost completion after truncation")
	}
	_, _ = s.Write([]byte("{\"type\":\"turn.failed\"}\n"))
	if s.Completed {
		t.Fatal("failed turn kept completion")
	}
}
