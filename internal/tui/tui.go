// Package tui is an asynchronous terminal client; all execution stays on the server.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/domain"
)

type Options struct {
	Spec              domain.Spec
	TokenFile, Output string
}

func Run(ctx context.Context, c *client.Client, opts Options) error {
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return fmt.Errorf("interactive terminal required; use dcar list --json for scripts")
	}
	if !validEndpoint(c.URL) {
		return fmt.Errorf("UI endpoint must be an HTTP(S) URL without embedded credentials, query or fragment")
	}
	if c.Token == "" {
		var tokens map[string]string
		b, err := os.ReadFile(opts.TokenFile)
		if err == nil {
			if err = json.Unmarshal(b, &tokens); err != nil {
				return fmt.Errorf("read token file: %w", err)
			}
			c.Token = tokens["local"]
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read token file: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newModel(ctx, c, opts)
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen()).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

type detail struct {
	Task     domain.Task      `json:"task"`
	Attempts []domain.Attempt `json:"attempts"`
}
type snapshotMsg struct {
	Client *client.Client
	Offset int
	ID     string
	Tasks  []domain.Task
	Detail detail
	Err    error
}
type detailMsg struct {
	Client *client.Client
	ID     string
	Detail detail
	Err    error
}
type actionMsg struct {
	Action, ID, Directory string
	Task                  domain.Task
	Count                 int
	Err                   error
}
type streamMsg struct {
	Generation int
	Event      domain.Event
	Task       domain.Task
	Err        error
	Done       bool
}
type reportMsg struct {
	Client      *client.Client
	ID, Content string
	Err         error
}
type tickMsg time.Time
type submission struct {
	Spec domain.Spec
	Key  string
}

type model struct {
	ctx                     context.Context
	client                  *client.Client
	opts                    Options
	width, height           int
	page                    string
	tasks                   []domain.Task
	selected, offset        int
	current                 detail
	viewport                viewport.Model
	tab                     int
	logs                    []string
	logBytes                int
	truncated               bool
	cursor                  int64
	generation              int
	stopStream              context.CancelFunc
	fetching, busy, healthy bool
	help                    bool
	notice, confirm, report string
	retryKeys               map[string]string
	pending                 *submission
	fields                  []textinput.Model
	prompt                  textarea.Model
	focus                   int
	connection              []textinput.Model
	connectionFocus         int
}

var fieldLabels = []string{"Repository", "Ref (optional)", "Profile", "Prepare command (optional)", "Test command (blank = detect)", "Credential reference (optional)", "Total timeout / seconds", "Test timeout / seconds (0 = default)"}
var accent = lipgloss.NewStyle().Foreground(lipgloss.Color("80")).Bold(true)
var muted = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
var selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Background(lipgloss.Color("80")).Bold(true)

func input(value string) textinput.Model {
	f := textinput.New()
	f.SetValue(value)
	f.CharLimit = 8192
	f.Prompt = "› "
	return f
}
func newModel(ctx context.Context, c *client.Client, opts Options) *model {
	s := opts.Spec
	if s.Profile == "" {
		s.Profile = "default"
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 3600
	}
	m := &model{ctx: ctx, client: c, opts: opts, width: 100, height: 32, page: "dashboard", viewport: viewport.New(60, 20), retryKeys: map[string]string{}}
	m.fields = []textinput.Model{input(s.Repository), input(s.Ref), input(s.Profile), input(s.PrepareCommand), input(s.TestCommand), input(s.CredentialRef), input(strconv.Itoa(s.TimeoutSeconds)), input(strconv.Itoa(s.TestTimeoutSeconds))}
	m.prompt = textarea.New()
	m.prompt.SetValue(s.Prompt)
	m.prompt.CharLimit = 65536
	m.prompt.SetHeight(5)
	m.prompt.ShowLineNumbers = false
	m.connection = []textinput.Model{input(c.URL), input(c.Token)}
	m.connection[1].EchoMode = textinput.EchoPassword
	m.connection[1].EchoCharacter = '•'
	if c.Token == "" {
		m.page = "connect"
		m.connection[0].Focus()
		m.notice = "Connect to your platform; API token stays in memory."
	}
	m.resize()
	return m
}
func ticker() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m *model) Init() tea.Cmd {
	if m.page == "connect" {
		return tea.Batch(ticker(), textinput.Blink)
	}
	return tea.Batch(ticker(), m.load())
}
func (m *model) selectedID() string {
	if m.current.Task.ID != "" {
		return m.current.Task.ID
	}
	return ""
}
func (m *model) accepts(t domain.Task) bool {
	current := m.current.Task
	if current.ID != t.ID {
		return true
	}
	if domain.Terminal(current.Status) && current.Status != t.Status {
		return false
	}
	return current.UpdatedAt.IsZero() || t.UpdatedAt.IsZero() || !t.UpdatedAt.Before(current.UpdatedAt)
}
func (m *model) load() tea.Cmd {
	if m.fetching {
		return nil
	}
	m.fetching = true
	offset, id, c, ctx := m.offset, m.selectedID(), m.client, m.ctx
	return func() tea.Msg {
		r := snapshotMsg{Client: c, Offset: offset, ID: id}
		r.Err = c.Do(ctx, "GET", fmt.Sprintf("/v1/tasks?limit=50&offset=%d", offset), nil, &r.Tasks, nil)
		if r.Err == nil && id != "" {
			r.Err = c.Do(ctx, "GET", "/v1/tasks/"+id, nil, &r.Detail, nil)
			var he *client.Error
			if errors.As(r.Err, &he) && he.Status == 404 {
				r.Err = nil
			}
		}
		return r
	}
}
func (m *model) fetchDetail(id string) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		r := detailMsg{Client: c, ID: id}
		r.Err = c.Do(ctx, "GET", "/v1/tasks/"+id, nil, &r.Detail, nil)
		return r
	}
}
func (m *model) selectTask(index int) tea.Cmd {
	if len(m.tasks) == 0 {
		return nil
	}
	index = max(0, min(index, len(m.tasks)-1))
	m.selected = index
	task := m.tasks[index]
	if task.ID == m.selectedID() {
		return nil
	}
	m.current = detail{Task: task}
	m.logs = nil
	m.logBytes = 0
	m.truncated = false
	m.cursor = 0
	m.report = ""
	m.viewport.GotoTop()
	m.content()
	return tea.Batch(m.startStream(), m.fetchDetail(task.ID))
}
func streamWait(ch <-chan streamMsg, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-ch:
			env := streamEnvelope{Msg: msg, Channel: ch, Context: ctx}
			for i := 0; i < 63; i++ {
				select {
				case next := <-ch:
					env.Extra = append(env.Extra, next)
				default:
					return env
				}
			}
			return env
		case <-ctx.Done():
			return nil
		}
	}
}

type streamEnvelope struct {
	Msg     streamMsg
	Extra   []streamMsg
	Channel <-chan streamMsg
	Context context.Context
}

func (m *model) startStream() tea.Cmd {
	if m.stopStream != nil {
		m.stopStream()
	}
	m.generation++
	if m.selectedID() == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.stopStream = cancel
	id, generation, after, c := m.selectedID(), m.generation, m.cursor, m.client
	ch := make(chan streamMsg, 64)
	send := func(msg streamMsg) {
		select {
		case ch <- msg:
		case <-ctx.Done():
		}
	}
	go func() {
		err := c.Events(ctx, id, after, true, func(e domain.Event) {
			e.Data = bounded(clean(e.Data), 8192)
			send(streamMsg{Generation: generation, Event: e})
		}, func(t domain.Task) { send(streamMsg{Generation: generation, Task: t}) })
		if ctx.Err() == nil {
			send(streamMsg{Generation: generation, Err: err, Done: true})
		}
	}()
	return streamWait(ch, ctx)
}
func (m *model) message(err error) {
	m.notice = err.Error()
	if m.client.Token != "" {
		m.notice = strings.ReplaceAll(m.notice, m.client.Token, "[redacted]")
	}
	m.notice = bounded(clean(m.notice), 1024)
}
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		m.content()
		return m, nil
	case tickMsg:
		if m.page == "connect" {
			return m, ticker()
		}
		return m, tea.Batch(ticker(), m.load())
	case snapshotMsg:
		if msg.Client != nil && msg.Client != m.client {
			return m, nil
		}
		m.fetching = false
		if msg.Offset != m.offset {
			return m, nil
		}
		if msg.Err != nil {
			m.healthy = false
			m.message(msg.Err)
			return m, nil
		}
		m.healthy = true
		id := m.selectedID()
		m.tasks = msg.Tasks
		if id == "" && len(m.tasks) > 0 {
			return m, m.selectTask(0)
		}
		found := false
		for i, t := range m.tasks {
			if t.ID == id {
				found = true
				m.selected = i
				if m.accepts(t) {
					m.current.Task = t
				} else {
					m.tasks[i] = m.current.Task
				}
			}
		}
		if !found && id != "" {
			m.current = detail{}
			if len(m.tasks) > 0 {
				return m, m.selectTask(min(m.selected, len(m.tasks)-1))
			}
			if m.stopStream != nil {
				m.stopStream()
			}
			m.generation++
			m.logs = nil
			m.report = ""
		}
		if msg.ID == id && msg.Detail.Task.ID == id && m.accepts(msg.Detail.Task) {
			m.current = msg.Detail
		}
		m.content()
		return m, nil
	case detailMsg:
		if msg.Client != nil && msg.Client != m.client {
			return m, nil
		}
		if msg.ID != m.selectedID() {
			return m, nil
		}
		if msg.Err != nil {
			m.message(msg.Err)
		} else if m.accepts(msg.Detail.Task) {
			m.current = msg.Detail
			m.content()
		}
		return m, nil
	case streamEnvelope:
		if msg.Msg.Generation != m.generation {
			return m, nil
		}
		done := false
		for _, e := range append([]streamMsg{msg.Msg}, msg.Extra...) {
			if e.Generation != m.generation {
				continue
			}
			done = done || e.Done
			if e.Err != nil {
				m.message(e.Err)
			}
			if e.Event.ID > m.cursor {
				m.cursor = e.Event.ID
				line := fmt.Sprintf("%d [%s] %s", e.Event.ID, clean(e.Event.Kind), e.Event.Data)
				m.logs = append(m.logs, line)
				m.logBytes += len(line)
				for len(m.logs) > 500 || m.logBytes > 512<<10 {
					m.logBytes -= len(m.logs[0])
					m.logs = m.logs[1:]
					m.truncated = true
				}
			}
			if e.Task.ID == m.selectedID() {
				m.current.Task = e.Task
			}
		}
		m.content()
		if done {
			return m, m.fetchDetail(m.selectedID())
		}
		return m, streamWait(msg.Channel, msg.Context)
	case actionMsg:
		m.busy = false
		if msg.Err != nil {
			m.message(msg.Err)
			var he *client.Error
			if errors.As(msg.Err, &he) && he.Status >= 400 && he.Status < 500 && he.Status != 408 && he.Status != 429 {
				if msg.Action == "submit" {
					m.pending = nil
				}
				if msg.Action == "retry" {
					delete(m.retryKeys, msg.ID)
				}
			}
			if msg.Action == "submit" && m.pending != nil {
				m.notice += " · Ctrl+S retries the SAME request; draft is locked until resolved."
			}
			return m, nil
		}
		m.notice = msg.Action + " completed"
		if msg.Action == "submit" || msg.Action == "retry" {
			m.pending = nil
			delete(m.retryKeys, msg.ID)
			m.page = "dashboard"
			m.tasks = append([]domain.Task{msg.Task}, m.tasks...)
			return m, tea.Batch(m.selectTask(0), m.load())
		}
		if msg.Action == "download" {
			m.notice = fmt.Sprintf("Verified %d artifacts → %s", msg.Count, msg.Directory)
		}
		return m, m.load()
	case reportMsg:
		if msg.Client != nil && msg.Client != m.client {
			return m, nil
		}
		if msg.ID != m.selectedID() {
			return m, nil
		}
		if msg.Err != nil {
			m.message(msg.Err)
		} else {
			m.report = clean(msg.Content)
			m.content()
		}
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" || (m.page == "dashboard" && key == "q" && m.confirm == "") {
			return m, tea.Quit
		}
		if m.page == "connect" {
			return m, m.connectKey(msg)
		}
		if m.page == "compose" {
			return m, m.formKey(msg)
		}
		if m.confirm != "" {
			if key == "y" || key == "enter" {
				action := m.confirm
				m.confirm = ""
				return m, m.action(action)
			}
			if key == "n" || key == "esc" {
				m.confirm = ""
			}
			return m, nil
		}
		if m.help {
			if key == "esc" || key == "?" {
				m.help = false
			}
			return m, nil
		}
		switch key {
		case "?":
			m.help = true
			return m, nil
		case "s":
			if m.busy || m.pending != nil {
				m.notice = "Resolve the pending request before changing connection."
				return m, nil
			}
			m.page = "connect"
			m.connection[0].SetValue(m.client.URL)
			m.connection[1].SetValue(m.client.Token)
			m.connectionFocus = 0
			m.connection[1].Blur()
			return m, m.connection[0].Focus()
		case "n":
			m.page = "compose"
			m.focus = 0
			m.focusForm()
			return m, textinput.Blink
		case "j", "down":
			return m, m.selectTask(m.selected + 1)
		case "k", "up":
			return m, m.selectTask(m.selected - 1)
		case "]", "[":
			if key == "]" && len(m.tasks) < 50 {
				return m, nil
			}
			if key == "[" && m.offset == 0 {
				return m, nil
			}
			if key == "]" {
				m.offset += 50
			} else {
				m.offset = max(0, m.offset-50)
			}
			m.current = detail{}
			m.tasks = nil
			if m.stopStream != nil {
				m.stopStream()
			}
			m.generation++
			m.logs = nil
			m.report = ""
			m.content()
			return m, m.load()
		case "tab":
			m.tab = (m.tab + 1) % 3
			m.viewport.GotoTop()
			m.content()
			if m.tab == 2 {
				return m, m.fetchReport()
			}
		case "ctrl+r":
			return m, tea.Batch(m.load(), m.startStream())
		case "c":
			if m.selectedID() != "" && !domain.Terminal(m.current.Task.Status) && !m.busy {
				m.confirm = "cancel"
			}
		case "r":
			if m.selectedID() != "" && domain.Terminal(m.current.Task.Status) && m.current.Task.Status != "succeeded" && !m.busy {
				m.confirm = "retry"
			}
		case "d":
			if m.selectedID() != "" && !m.busy {
				return m, m.action("download")
			}
		case "end":
			m.viewport.GotoBottom()
			return m, nil
		case "home":
			m.viewport.GotoTop()
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	// Cursor blink/paste messages must reach the focused component too.
	if m.page == "compose" && m.pending == nil {
		return m, m.updateForm(msg)
	}
	if m.page == "connect" {
		var cmd tea.Cmd
		m.connection[m.connectionFocus], cmd = m.connection[m.connectionFocus].Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) action(action string) tea.Cmd {
	if m.busy {
		return nil
	}
	m.busy = true
	id, c, ctx := m.selectedID(), m.client, m.ctx
	key := ""
	if action == "retry" {
		key = m.retryKeys[id]
		if key == "" {
			key = domain.ID()
			m.retryKeys[id] = key
		}
	}
	dir := filepath.Join(m.opts.Output, id+"-"+time.Now().Format("20060102-150405.000"))
	return func() tea.Msg {
		r := actionMsg{Action: action, ID: id, Directory: dir}
		if action == "download" {
			r.Count, r.Err = c.Download(ctx, id, dir)
		} else {
			r.Err = c.Do(ctx, "POST", "/v1/tasks/"+id+"/"+action, nil, &r.Task, map[string]string{"Idempotency-Key": key})
		}
		return r
	}
}
func (m *model) fetchReport() tea.Cmd {
	id, c, ctx := m.selectedID(), m.client, m.ctx
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		r := reportMsg{Client: c, ID: id}
		var raw json.RawMessage
		r.Err = c.Do(ctx, "GET", "/v1/tasks/"+id+"/report", nil, &raw, nil)
		if r.Err == nil {
			var v any
			if r.Err = json.Unmarshal(raw, &v); r.Err == nil {
				b, _ := json.MarshalIndent(v, "", "  ")
				r.Content = bounded(string(b), 512<<10)
			}
		}
		return r
	}
}
func (m *model) connectKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "tab", "shift+tab":
		m.connection[m.connectionFocus].Blur()
		m.connectionFocus = 1 - m.connectionFocus
		return m.connection[m.connectionFocus].Focus()
	case "enter":
		endpoint := strings.TrimRight(strings.TrimSpace(m.connection[0].Value()), "/")
		if !validEndpoint(endpoint) {
			m.notice = "Enter an HTTP(S) API URL without embedded credentials."
			return nil
		}
		token := strings.TrimSpace(m.connection[1].Value())
		if token == "" {
			m.notice = "API token is required."
			return nil
		}
		if m.stopStream != nil {
			m.stopStream()
		}
		m.generation++
		m.client = client.New(endpoint, token)
		m.tasks = nil
		m.current = detail{}
		m.logs = nil
		m.report = ""
		m.offset = 0
		m.selected = 0
		m.fetching = false
		m.healthy = false
		m.page = "dashboard"
		m.notice = "Connecting…"
		return m.load()
	}
	var cmd tea.Cmd
	m.connection[m.connectionFocus], cmd = m.connection[m.connectionFocus].Update(msg)
	return cmd
}
func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// Form focus: repository, multiline prompt, then the remaining optional fields.
func (m *model) focusForm() {
	for i := range m.fields {
		m.fields[i].Blur()
	}
	m.prompt.Blur()
	if m.focus == 1 {
		m.prompt.Focus()
	} else {
		i := m.focus
		if i > 1 {
			i--
		}
		m.fields[i].Focus()
	}
}
func (m *model) updateForm(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.focus == 1 {
		m.prompt, cmd = m.prompt.Update(msg)
	} else {
		i := m.focus
		if i > 1 {
			i--
		}
		m.fields[i], cmd = m.fields[i].Update(msg)
	}
	return cmd
}
func (m *model) formKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.page = "dashboard"
		return nil
	case "ctrl+s":
		if m.busy {
			return nil
		}
		if m.pending == nil {
			total, err := strconv.Atoi(m.fields[6].Value())
			if err != nil {
				m.notice = "Total timeout must be an integer."
				return nil
			}
			test, err := strconv.Atoi(m.fields[7].Value())
			if err != nil {
				m.notice = "Test timeout must be an integer."
				return nil
			}
			s := domain.Spec{Repository: m.fields[0].Value(), Prompt: m.prompt.Value(), Ref: m.fields[1].Value(), Profile: m.fields[2].Value(), PrepareCommand: m.fields[3].Value(), TestCommand: m.fields[4].Value(), CredentialRef: m.fields[5].Value(), TimeoutSeconds: total, TestTimeoutSeconds: test}
			if err = s.Normalize(); err != nil {
				m.message(err)
				return nil
			}
			m.pending = &submission{Spec: s, Key: domain.ID()}
		}
		m.busy = true
		pending, c, ctx := *m.pending, m.client, m.ctx
		return func() tea.Msg {
			r := actionMsg{Action: "submit"}
			r.Err = c.Do(ctx, http.MethodPost, "/v1/tasks", pending.Spec, &r.Task, map[string]string{"Idempotency-Key": pending.Key})
			return r
		}
	case "tab", "shift+tab":
		if m.pending != nil {
			return nil
		}
		delta := 1
		if msg.String() == "shift+tab" {
			delta = -1
		}
		m.focus = (m.focus + delta + 9) % 9
		m.focusForm()
		return textinput.Blink
	}
	if m.pending != nil {
		return nil
	}
	return m.updateForm(msg)
}

func clean(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
}
func bounded(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + " [truncated]"
}
func (m *model) resize() {
	left := 30
	m.viewport.Width = max(20, m.width-left-7)
	m.viewport.Height = max(5, m.height-9)
	for i := range m.fields {
		m.fields[i].Width = max(20, m.width-10)
	}
	for i := range m.connection {
		m.connection[i].Width = max(20, m.width-12)
	}
	m.prompt.SetWidth(max(20, m.width-8))
}
func (m *model) content() {
	bottom := m.viewport.AtBottom()
	var text string
	switch m.tab {
	case 1:
		text = strings.Join(m.logs, "\n")
		if text == "" {
			text = "Waiting for execution events…"
		}
		if m.truncated {
			text = "[Older events trimmed from display; download execution.log for retained full logs.]\n" + text
		}
	case 2:
		text = m.report
		if text == "" {
			text = "Press Tab to open the control-plane report. Download for patch and detailed reports."
		}
	default:
		t := m.current.Task
		if t.ID == "" {
			text = "No task selected.\n\nPress n to describe a new coding task."
		} else {
			text = fmt.Sprintf("%s\n%s\n\n%s\n\nStatus     %s\nStage      %s\nDeadline   %s\nBaseline   %s\n\n%s", t.ID, clean(t.Spec.Repository), clean(t.Spec.Prompt), t.Status, t.Stage, t.Deadline.Local().Format(time.RFC3339), t.SHA, clean(t.Error))
			for _, a := range m.current.Attempts {
				text += fmt.Sprintf("\n\nAttempt %d · %s · %s\nWorker %s\n%s", a.Fence, a.Status, a.Stage, clean(a.WorkerID), clean(a.Error))
				if v := a.Verification; v != nil {
					text += fmt.Sprintf("\nVerification: %s · repair rounds %d", v.State, v.RepairRounds)
					for _, test := range v.Tests {
						text += fmt.Sprintf("\n  %s · exit %d · %dms · timeout %t", clean(test.Command), test.ExitCode, test.DurationMS, test.TimedOut)
					}
					if len(v.TestsModified) > 0 {
						text += "\nModified tests: " + clean(strings.Join(v.TestsModified, ", "))
					}
				}
			}
		}
	}
	text = lipgloss.NewStyle().Width(m.viewport.Width).Render(text)
	m.viewport.SetContent(text)
	if bottom && m.tab == 1 {
		m.viewport.GotoBottom()
	}
}
func (m *model) View() string {
	if m.width < 70 || m.height < 20 {
		return "DCAR · Enlarge your terminal to at least 70 × 20.\nCtrl+C to exit."
	}
	state := "CONNECTING"
	if m.healthy {
		state = "ONLINE"
	}
	header := accent.Render(" DCAR ") + muted.Render(" / DISTRIBUTED AGENTS ") + accent.Render(state) + muted.Render(" · "+ansi.Truncate(clean(m.client.URL), max(10, m.width-43), "…"))
	footerText := strings.ReplaceAll(clean(m.notice), "\n", " ")
	if m.busy {
		footerText = "Working… " + footerText
	}
	footer := muted.Render(ansi.Truncate(footerText, m.width-2, "…"))
	if m.page == "connect" {
		return header + "\n\n" + accent.Render("Connect to your runtime") + "\n\nAPI URL\n" + m.connection[0].View() + "\n\nAPI token\n" + m.connection[1].View() + "\n\n" + muted.Render("Tab switch · Enter connect · Ctrl+C quit") + "\n\n" + footer
	}
	if m.page == "compose" {
		// Show a compact basic form, then scroll the optional settings into view with focus.
		var body string
		if m.focus <= 1 {
			body = accent.Render("Repository") + "\n" + m.fields[0].View() + "\n\n" + accent.Render("Describe the coding task") + "\n" + m.prompt.View() + "\n\n" + muted.Render("Tab to optional execution settings; Enter adds a line to the task.")
		} else {
			start := max(1, m.focus-3)
			end := min(8, start+max(2, (m.height-10)/3))
			if m.focus-1 >= end {
				start = m.focus - 1
				end = min(8, start+2)
			}
			for i := start; i < end; i++ {
				body += accent.Render(fieldLabels[i]) + "\n" + m.fields[i].View() + "\n\n"
			}
		}
		key := ""
		if m.pending != nil {
			key = " · frozen key " + m.pending.Key
		}
		return header + "\n\n" + accent.Render("New task") + key + "\n\n" + body + "\n" + muted.Render("Tab / Shift+Tab fields · Ctrl+S submit · Esc back · Ctrl+C quit") + "\n\n" + footer
	}
	if m.help {
		return header + "\n\n" + accent.Render("Keyboard shortcuts") + "\n\n" + strings.Join([]string{
			"n                Create a task",
			"↑ / ↓ or j / k   Select a task",
			"Tab              Details → live logs → report",
			"PgUp / PgDn      Scroll the active panel",
			"End / Home       Follow log tail / jump to top",
			"c / r            Cancel / retry (confirm before sending)",
			"d                Download artifacts and verify checksums",
			"[ / ]            Previous / next page (50 tasks per page)",
			"s / Ctrl+R       Change connection / reconnect stream",
			"q / Ctrl+C       Quit; remote tasks continue running",
			"Esc / ?          Close this help",
		}, "\n") + "\n\n" + footer
	}
	left := fmt.Sprintf("TASKS · %d–%d", m.offset+1, m.offset+len(m.tasks))
	if len(m.tasks) == 0 {
		left = "TASKS · no tasks"
	}
	visible := m.viewport.Height - 2
	start := max(0, m.selected-visible+1)
	for i := start; i < min(len(m.tasks), start+visible); i++ {
		t := m.tasks[i]
		repo := strings.TrimSuffix(strings.TrimPrefix(t.Spec.Repository, "https://github.com/"), ".git")
		row := ansi.Truncate(fmt.Sprintf("%s %s", t.Status, clean(repo)), 28, "…")
		if i == m.selected {
			row = selectedStyle.Width(28).Render(row)
		}
		left += "\n" + row
	}
	pane := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)
	tabs := []string{"DETAILS", "LIVE LOGS", "REPORT"}
	for i := range tabs {
		if i == m.tab {
			tabs[i] = accent.Render(tabs[i])
		} else {
			tabs[i] = muted.Render(tabs[i])
		}
	}
	right := strings.Join(tabs, "  ") + "\n" + m.viewport.View()
	main := lipgloss.JoinHorizontal(lipgloss.Top, pane.Width(28).Height(m.viewport.Height+1).Render(left), pane.Width(m.viewport.Width).Render(right))
	help := "n new · ↑↓ tasks · Tab panel · c cancel · r retry · d download · ? help · q quit"
	if m.confirm != "" {
		help = "Confirm " + m.confirm + " for " + m.selectedID() + "?  y / Enter confirm · n / Esc back"
	}
	return header + "\n" + main + "\n" + muted.Render(ansi.Truncate(help, m.width-1, "…")) + "\n" + footer
}
