package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Uses the real pinned CLI with a local Responses fixture; it does not verify model quality or credentials.
func TestPinnedCodexCLIContract(t *testing.T) {
	binary := os.Getenv("CODEX_CONTRACT_BINARY")
	if binary == "" {
		t.Skip("CODEX_CONTRACT_BINARY unset: actual pinned Codex contract not executed")
	}
	version, e := exec.Command(binary, "--version").CombinedOutput()
	if e != nil || !strings.Contains(string(version), "0.114.0") {
		t.Fatalf("expected pinned Codex 0.114.0: %s %v", version, e)
	}
	for _, model := range []string{"gpt-5.4", "glm-5.3"} {
		t.Run(model, func(t *testing.T) { pinnedCodexContract(t, binary, model) })
	}
}

func pinnedCodexContract(t *testing.T, binary, model string) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer contract-attempt" {
			http.Error(w, "bad contract", 400)
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != model {
			http.Error(w, "bad model", 400)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		text := map[string]any{"type": "output_text", "text": "contract ready", "annotations": []any{}}
		message := map[string]any{"id": "msg_contract", "type": "message", "role": "assistant", "status": "completed", "content": []any{text}}
		response := map[string]any{"id": "resp_contract", "object": "response", "status": "completed", "model": model, "output": []any{message}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}}
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_contract", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_contract", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
			{"type": "response.output_text.delta", "item_id": "msg_contract", "output_index": 0, "content_index": 0, "delta": "contract ready"},
			{"type": "response.output_item.done", "output_index": 0, "item": message},
			{"type": "response.completed", "response": response},
		} {
			b, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], b)
		}
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	catalog := ""
	if model == "glm-5.3" {
		var err error
		catalog, err = filepath.Abs("../../config/codex-bigmodel-models.json")
		if err != nil {
			t.Fatal(err)
		}
	}
	args := CodexArgs(upstream.URL+"/v1", model, catalog)
	args = append(args[:len(args)-1], "--skip-git-repo-check", "-")
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = t.TempDir()
	home := filepath.Join(t.TempDir(), "codex-home")
	if e := os.Mkdir(home, 0700); e != nil {
		t.Fatal(e)
	}
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "DCAR_ATTEMPT_TOKEN=contract-attempt")
	cmd.Stdin = strings.NewReader("Reply contract ready. Do not call tools.")
	out, e := cmd.CombinedOutput()
	stream := &AgentStream{Output: &Limited{Limit: 8 << 20}}
	_, _ = stream.Write(out)
	if e != nil || !stream.Completed || requests.Load() != 1 || !strings.Contains(string(out), "contract ready") {
		t.Fatalf("pinned CLI contract failed: %v\n%s", e, out)
	}
}
