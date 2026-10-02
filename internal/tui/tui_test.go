package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/domain"
)

func testModel(t *testing.T, handler http.HandlerFunc) *model {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newModel(ctx, client.New(s.URL, "secret-token"), Options{Spec: domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: "修复代码并执行测试"}, Output: t.TempDir()})
}
func key(s string) tea.KeyMsg {
	switch s {
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestPanelScrollingAndLiveTail(t *testing.T) {
	for _, tab := range []int{1, 2} {
		t.Run(fmt.Sprint(tab), func(t *testing.T) {
			m := testModel(t, func(w http.ResponseWriter, r *http.Request) {})
			m.current.Task = domain.Task{ID: "selected", Status: "running"}
			m.tasks = []domain.Task{m.current.Task, {ID: "other"}}
			m.tab = tab
			for i := 0; i < 100; i++ {
				m.logs = append(m.logs, fmt.Sprintf("line %d", i))
			}
			m.report = strings.Join(m.logs, "\n")
			m.content()
			m.Update(key("end"))
			before := m.viewport.YOffset
			m.Update(key("up"))
			if m.selectedID() != "selected" || m.viewport.YOffset != before-1 || m.followLogs {
				t.Fatal("up must scroll the panel and pause following, not change tasks")
			}
			before = m.viewport.YOffset
			m.logs = append(m.logs, "new event")
			m.content()
			if m.viewport.YOffset != before {
				t.Fatal("content refresh discarded manual scroll position")
			}
			m.Update(key("pgup"))
			if m.viewport.YOffset >= before {
				t.Fatal("page up did not move")
			}
			m.Update(key("home"))
			m.Update(key("down"))
			if m.viewport.YOffset != 1 {
				t.Fatal("down did not scroll one line")
			}
			m.Update(key("pgdown"))
			if m.viewport.YOffset <= 1 {
				t.Fatal("page down did not move")
			}
			before = m.viewport.YOffset
			m.Update(tea.MouseMsg{X: 40, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
			if m.viewport.YOffset >= before || m.followLogs {
				t.Fatal("wheel did not scroll or pause tail")
			}
			before = m.viewport.YOffset
			m.Update(tea.MouseMsg{X: 5, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
			if m.viewport.YOffset != before {
				t.Fatal("wheel over task list scrolled the right panel")
			}
			m.Update(key("end"))
			m.logs = append(m.logs, "latest event")
			m.content()
			if !m.viewport.AtBottom() || !m.followLogs {
				t.Fatal("End did not resume tail")
			}
			m.Update(key("j"))
			if m.selectedID() != "other" {
				t.Fatal("j must still select the next task")
			}
		})
	}
}

func TestHomePausesTailBeforeLogOverflows(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {})
	m.tab = 1
	m.logs = []string{"first"}
	m.content()
	m.Update(key("home"))
	m.logs = append(m.logs, strings.Repeat("next\n", 100))
	m.content()
	if m.viewport.YOffset != 0 || m.followLogs {
		t.Fatal("short log at Home started following on overflow")
	}
}

func TestSubmissionReusesFrozenRequestAfterLostResponse(t *testing.T) {
	var keys []string
	var specs []domain.Spec
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		var s domain.Spec
		_ = json.NewDecoder(r.Body).Decode(&s)
		specs = append(specs, s)
		if len(keys) == 1 {
			http.Error(w, "temporary error secret-token", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(domain.Task{ID: strings.Repeat("a", 32), Status: "queued", Spec: s})
	})
	m.page = "compose"
	cmd := m.formKey(key("ctrl+s"))
	if cmd == nil {
		t.Fatal("no submission")
	}
	m.Update(cmd())
	if m.pending == nil || m.busy || strings.Contains(m.notice, "secret-token") {
		t.Fatalf("lost response must retain request and redact token: %s", m.notice)
	}
	original := m.prompt.Value()
	m.focus = 1
	m.formKey(key("x"))
	if m.prompt.Value() != original {
		t.Fatal("uncertain submission draft was editable")
	}
	cmd = m.formKey(key("ctrl+s"))
	result := cmd().(actionMsg)
	if result.Err != nil || len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] || specs[0] != specs[1] {
		t.Fatalf("duplicate request protection violated: %+v", keys)
	}
	// Do not execute follow-up network commands returned by Update.
	m.Update(result)
	if m.pending != nil || m.page != "dashboard" || m.selectedID() != result.Task.ID {
		t.Fatal("successful submission did not select task")
	}
}

func TestTerminalMutationRequiresConfirmationAndRetryKeepsKey(t *testing.T) {
	var calls, keys []string
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		http.Error(w, "transient", 503)
	})
	m.current.Task = domain.Task{ID: strings.Repeat("a", 32), Status: "running"}
	_, cmd := m.Update(key("c"))
	if cmd != nil || m.confirm != "cancel" || len(calls) != 0 {
		t.Fatal("cancel ran without confirmation")
	}
	m.Update(key("esc"))
	if m.confirm != "" {
		t.Fatal("confirmation not dismissed")
	}
	m.current.Task.Status = "failed"
	m.Update(key("r"))
	_, cmd = m.Update(key("enter"))
	m.Update(cmd())
	m.Update(key("r"))
	_, cmd = m.Update(key("enter"))
	m.Update(cmd())
	if len(calls) != 2 || keys[0] == "" || keys[0] != keys[1] || !strings.HasSuffix(calls[0], "/retry") {
		t.Fatal("retry used different idempotency keys")
	}
}

func TestStaleResponsesAndBoundedSanitizedLogs(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {})
	m.current.Task = domain.Task{ID: "new-task", Status: "running"}
	m.generation = 2
	m.tab = 1
	m.Update(detailMsg{ID: "old-task", Detail: detail{Task: domain.Task{ID: "old-task"}}})
	m.Update(snapshotMsg{Client: client.New("http://old-api", "old-token"), Tasks: []domain.Task{{ID: "old-task"}}})
	m.Update(streamEnvelope{Msg: streamMsg{Generation: 1, Event: domain.Event{ID: 100, Data: "stale"}}})
	if m.selectedID() != "new-task" || len(m.logs) != 0 {
		t.Fatal("stale response changed selection")
	}
	for i := 1; i <= 700; i += 50 {
		env := streamEnvelope{Msg: streamMsg{Generation: 2}}
		for j := i; j < min(701, i+50); j++ {
			env.Extra = append(env.Extra, streamMsg{Generation: 2, Event: domain.Event{ID: int64(j), Kind: "shell", Data: clean("\x1b[2J\x1b]52;c;secret\a内容\n" + strings.Repeat("x", 2048))}})
		}
		m.Update(env)
	}
	if !m.truncated || len(m.logs) > 500 || m.logBytes > 512<<10 || m.cursor != 700 {
		t.Fatal("unbounded or missing logs")
	}
	if strings.Contains(strings.Join(m.logs, ""), "\x1b") {
		t.Fatal("repository output can inject terminal control sequences")
	}
	before := len(m.logs)
	m.Update(streamEnvelope{Msg: streamMsg{Generation: 2, Event: domain.Event{ID: 700, Data: "duplicate"}}})
	if len(m.logs) != before {
		t.Fatal("duplicate event displayed")
	}
	truncated := bounded(strings.Repeat("中文", 10000), 8192)
	if !utf8.ValidString(truncated) {
		t.Fatal("truncated text is not valid UTF-8")
	}
}

func TestLayoutsAndPasswordMasking(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {})
	m.notice = "HTTP error:\n<html>\nconnection unavailable"
	m.busy = true
	m.pending = &submission{Key: strings.Repeat("a", 32)}
	m.tasks = []domain.Task{{ID: "task", Status: "running", Spec: domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: "修改代码"}, Deadline: time.Now().Add(time.Hour)}}
	m.current.Task = m.tasks[0]
	for _, size := range [][2]int{{70, 20}, {80, 24}, {100, 32}, {140, 45}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, page := range []string{"dashboard", "connect", "compose"} {
			m.page = page
			for focus := 0; focus < 9; focus++ {
				m.focus = focus
				view := m.View()
				lines := strings.Split(view, "\n")
				if strings.Contains(view, "secret-token") {
					t.Fatal("token visible")
				}
				if len(lines) > size[1] {
					t.Errorf("%s focus %d: %d lines exceed %d", page, focus, len(lines), size[1])
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > size[0] {
						t.Errorf("%s width %d exceeds %d: %s", page, ansi.StringWidth(line), size[0], line)
					}
				}
			}
		}
	}
	m.page = "dashboard"
	m.help = true
	if len(strings.Split(m.View(), "\n")) > m.height {
		t.Fatal("help exceeds terminal height")
	}
	m.width = 60
	if !strings.Contains(m.View(), "Enlarge") {
		t.Fatal("missing small terminal guidance")
	}
}

func TestSSEStreamDeliversTerminalAndStopsOnSelectionChange(t *testing.T) {
	m := testModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: execution\ndata: {\"id\":1,\"kind\":\"agent\",\"data\":\"working\"}\n\n")
		fmt.Fprint(w, "event: terminal\ndata: {\"id\":\"task\",\"status\":\"succeeded\"}\n\n")
	})
	m.current.Task = domain.Task{ID: "task", Status: "running"}
	cmd := m.startStream()
	for i := 0; i < 3; i++ {
		msg := cmd()
		env := msg.(streamEnvelope)
		_, cmd = m.Update(env)
		if env.Msg.Done || len(env.Extra) > 0 && env.Extra[len(env.Extra)-1].Done {
			break
		}
	}
	if m.current.Task.Status != "succeeded" || m.cursor != 1 || len(m.logs) != 1 {
		t.Fatal("SSE results not applied")
	}
	m.Update(detailMsg{ID: "task", Detail: detail{Task: domain.Task{ID: "task", Status: "running"}}})
	if m.current.Task.Status != "succeeded" {
		t.Fatal("delayed HTTP response reverted terminal result")
	}
	m.stopStream()
}
