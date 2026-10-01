package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/domain"
)

type observedState struct {
	Task   domain.Task
	Notice string
	Cursor int64
}
type observer struct {
	inner  *model
	states chan observedState
}

func (o *observer) Init() tea.Cmd { return o.inner.Init() }
func (o *observer) View() string  { return o.inner.View() }
func (o *observer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := o.inner.Update(msg)
	select {
	case o.states <- observedState{Task: o.inner.current.Task, Notice: o.inner.notice, Cursor: o.inner.cursor}:
	default:
	}
	return o, cmd
}

// Exercise the real Bubble Tea command/message loop with actual HTTP and SSE,
// without a terminal renderer or a Docker/model dependency.
func TestProgramSubmitStreamDownloadAndQuit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var submitted atomic.Bool
	task := domain.Task{ID: strings.Repeat("a", 32), Status: "succeeded", Spec: domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: "修复代码"}}
	patch := []byte("verified patch")
	hash := sha256.Sum256(patch)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ui-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/tasks":
			var spec domain.Spec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || r.Header.Get("Idempotency-Key") == "" || spec.Prompt != "修复代码" {
				http.Error(w, "bad submission", 400)
				return
			}
			submitted.Store(true)
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(task)
		case r.URL.Path == "/v1/tasks":
			if submitted.Load() {
				_ = json.NewEncoder(w).Encode([]domain.Task{task})
			} else {
				fmt.Fprint(w, "[]")
			}
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: execution\ndata: {\"id\":1,\"kind\":\"test\",\"data\":\"passed\"}\n\n")
			raw, _ := json.Marshal(task)
			fmt.Fprintf(w, "event: terminal\ndata: %s\n\n", raw)
		case strings.HasSuffix(r.URL.Path, "/artifacts"):
			_ = json.NewEncoder(w).Encode([]domain.Artifact{{Name: "changes.patch", Size: int64(len(patch)), SHA256: hex.EncodeToString(hash[:])}})
		case strings.HasSuffix(r.URL.Path, "/changes.patch"):
			_, _ = w.Write(patch)
		default:
			_ = json.NewEncoder(w).Encode(detail{Task: task})
		}
	}))
	defer server.Close()
	inner := newModel(ctx, client.New(server.URL, "ui-token"), Options{Spec: task.Spec, Output: t.TempDir()})
	obs := &observer{inner: inner, states: make(chan observedState, 64)}
	p := tea.NewProgram(obs, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	defer p.Kill()
	p.Send(key("n"))
	p.Send(key("ctrl+s"))
	wait := func(predicate func(observedState) bool) {
		t.Helper()
		for {
			select {
			case state := <-obs.states:
				if predicate(state) {
					return
				}
			case <-ctx.Done():
				t.Fatal("UI flow did not complete")
			}
		}
	}
	wait(func(s observedState) bool {
		return s.Task.ID == task.ID && s.Task.Status == "succeeded" && s.Cursor == 1
	})
	p.Send(key("d"))
	wait(func(s observedState) bool { return strings.HasPrefix(s.Notice, "Verified 1 artifacts") })
	p.Send(key("q"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("program failed to quit")
	}
	if !submitted.Load() || inner.cursor != 1 {
		t.Fatal("HTTP submission or SSE event was not delivered")
	}
}
