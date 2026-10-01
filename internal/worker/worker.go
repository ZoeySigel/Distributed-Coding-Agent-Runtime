package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/control"
	"github.com/dcar/runtime/internal/docker"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/runner"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Interfaces expose process/runtime boundaries without duplicating the agent's own tool system.
type WorkspaceRuntime interface {
	Create(context.Context, docker.Container) (string, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Remove(context.Context, string) error
	Copy(context.Context, string, string, string, []byte) error
	Exec(context.Context, string, []string, []string, int) (docker.Result, error)
}
type AgentExecutor interface {
	Execute(context.Context, string, runner.Input) (runner.AgentResult, error)
}
type RepositoryProvider interface {
	Prepare(context.Context, string, runner.Input) (runner.Prepared, error)
}
type TestRunner interface {
	Test(context.Context, string, runner.Input) (domain.TestResult, error)
}
type ContainerExecutor struct {
	Runtime WorkspaceRuntime
	Observe func(string)
}

func (x *ContainerExecutor) invoke(ctx context.Context, id, op string, in runner.Input, out any) error {
	b, _ := json.Marshal(in)
	name := "input-" + domain.ID() + ".json"
	if e := x.Runtime.Copy(ctx, id, "/run/dcar", name, b); e != nil {
		return e
	}
	argv := []string{"/usr/local/bin/dcar-workspace", op, "/run/dcar/" + name}
	var res docker.Result
	var e error
	if native, ok := x.Runtime.(*docker.Engine); ok && op == "agent" && x.Observe != nil {
		observer := &lineObserver{emit: x.Observe}
		res, e = native.ExecObserved(ctx, id, argv, nil, 48<<20, observer.write)
	} else {
		res, e = x.Runtime.Exec(ctx, id, argv, nil, 48<<20)
	}
	if e != nil {
		return e
	}
	if res.Code != 0 {
		if res.Code >= 128 {
			return fmt.Errorf("workspace process interrupted (exit %d)", res.Code)
		}
		return fmt.Errorf("%s: %s", op, redact(string(res.Output), in.Credential))
	}
	if res.Truncated {
		return fmt.Errorf("runner output exceeded limit")
	}
	return runner.Parse(res.Output, out)
}

type lineObserver struct {
	line []byte
	skip bool
	emit func(string)
}

func (o *lineObserver) write(b []byte) {
	for _, c := range b {
		if c == '\n' {
			if !o.skip && len(o.line) > 0 && !bytes.HasPrefix(o.line, []byte(runner.Marker)) {
				o.emit(string(o.line))
			}
			o.line = o.line[:0]
			o.skip = false
			continue
		}
		if o.skip {
			continue
		}
		o.line = append(o.line, c)
		if len(o.line) == len(runner.Marker) && string(o.line) == runner.Marker {
			o.skip = true
			o.line = o.line[:0]
		}
		if len(o.line) >= 60000 {
			o.emit(string(o.line) + " [LINE TRUNCATED]")
			o.skip = true
			o.line = o.line[:0]
		}
	}
}
func (x *ContainerExecutor) Execute(ctx context.Context, id string, in runner.Input) (runner.AgentResult, error) {
	var v runner.AgentResult
	e := x.invoke(ctx, id, "agent", in, &v)
	return v, e
}
func (x *ContainerExecutor) Prepare(ctx context.Context, id string, in runner.Input) (runner.Prepared, error) {
	var v runner.Prepared
	e := x.invoke(ctx, id, "prepare", in, &v)
	return v, e
}
func (x *ContainerExecutor) Test(ctx context.Context, id string, in runner.Input) (domain.TestResult, error) {
	var v domain.TestResult
	e := x.invoke(ctx, id, "shell", in, &v)
	return v, e
}
func redact(s string, secrets ...string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("attempt:"+v)), "[REDACTED]")
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

type Worker struct {
	Control       *client.Client
	Engine        *docker.Engine
	Registration  domain.Registration
	Gateway       string
	mu            sync.Mutex
	active        map[string]bool
	CleanupErrors atomic.Int64
	Completed     atomic.Int64
	TestPassed    atomic.Int64
	TestFailed    atomic.Int64
	Interrupted   atomic.Int64
	StopMillis    atomic.Int64
	CancelMillis  atomic.Int64
	CancelCount   atomic.Int64
}
type assignment struct {
	Lease   domain.Lease   `json:"lease"`
	Profile domain.Profile `json:"profile"`
}

func (w *Worker) Run(ctx context.Context) error {
	if e := w.Engine.Ping(ctx); e != nil {
		return e
	}
	if e := w.Control.Do(ctx, "POST", "/internal/register", w.Registration, nil, nil); e != nil {
		return e
	}
	w.mu.Lock()
	w.active = map[string]bool{}
	w.mu.Unlock()
	w.sweep(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	gc := time.NewTicker(30 * time.Second)
	defer gc.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-gc.C:
			w.sweep(ctx)
		case <-tick.C:
			for {
				w.mu.Lock()
				n := len(w.active)
				w.mu.Unlock()
				if n >= w.Registration.Capacity {
					break
				}
				var a assignment
				e := w.Control.Do(ctx, "POST", "/internal/claim", w.Registration, &a, nil)
				if e != nil {
					var he *client.Error
					if errors.As(e, &he) && he.Status == 409 {
						return e
					}
					slog.Error("claim", "error", e)
					break
				}
				if a.Lease.Attempt.ID == "" {
					break
				}
				w.mu.Lock()
				w.active[a.Lease.Attempt.ID] = true
				w.mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { w.mu.Lock(); delete(w.active, a.Lease.Attempt.ID); w.mu.Unlock() }()
					w.execute(ctx, a)
				}()
			}
		}
	}
}
func proof(l domain.Lease) domain.Proof {
	return domain.Proof{AttemptID: l.Attempt.ID, WorkerID: l.Attempt.WorkerID, Session: l.Attempt.Session, Fence: l.Attempt.Fence}
}

type Report struct {
	TaskID            string              `json:"task_id"`
	AttemptID         string              `json:"attempt_id"`
	Fence             int64               `json:"fence"`
	SHA               string              `json:"sha"`
	Image             string              `json:"image"`
	Executor          string              `json:"executor"`
	Model             string              `json:"model"`
	AgentVersion      string              `json:"agent_version"`
	Status            string              `json:"status"`
	Error             string              `json:"error,omitempty"`
	Verification      string              `json:"verification"`
	Baseline          *domain.TestResult  `json:"baseline,omitempty"`
	Tests             []domain.TestResult `json:"tests"`
	RepairRounds      int                 `json:"repair_rounds"`
	Files             []string            `json:"files"`
	TestsModified     []string            `json:"tests_modified"`
	LogTruncated      bool                `json:"log_truncated"`
	ArtifactsComplete bool                `json:"artifacts_complete"`
	StartedAt         time.Time           `json:"started_at"`
	FinishedAt        time.Time           `json:"finished_at"`
	Note              string              `json:"note"`
	Attempts          []domain.Attempt    `json:"attempts"`
}

func (w *Worker) execute(parent context.Context, a assignment) {
	l, p := a.Lease, proof(a.Lease)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		safeUntil := time.Now().Add(time.Duration(l.TTLSeconds-5) * time.Second)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				started := time.Now()
				renewCtx, endRenew := context.WithDeadline(ctx, safeUntil)
				e := w.Control.Do(renewCtx, "POST", "/internal/update", control.UpdateRequest{Proof: p, Renew: true}, nil, nil)
				endRenew()
				if e == nil {
					safeUntil = started.Add(time.Duration(l.TTLSeconds-5) * time.Second)
				} else {
					var he *client.Error
					if (errors.As(e, &he) && he.Status == 409) || time.Now().After(safeUntil) {
						cancel()
						return
					}
				}
			}
		}
	}()
	defer func() { cancel(); <-heartbeatDone }()
	aid := l.Attempt.ID
	labels := map[string]string{"dcar.worker": p.WorkerID, "dcar.session": p.Session, "dcar.attempt": aid, "dcar.fence": strconv.FormatInt(p.Fence, 10)}
	var netID string
	volumes := []string{}
	containers := []string{}
	cleanup := func() {
		start := time.Now()
		cleaned := true
		cleanupCtx, c := context.WithTimeout(context.Background(), 45*time.Second)
		defer c()
		for _, id := range containers {
			_ = w.Engine.Stop(cleanupCtx, id)
			if e := w.Engine.Remove(cleanupCtx, id); e != nil {
				cleaned = false
				w.cleanupError(e)
			}
		}
		for _, v := range volumes {
			if e := w.Engine.RemoveVolume(cleanupCtx, v); e != nil {
				cleaned = false
				w.cleanupError(e)
			}
		}
		if netID != "" {
			if e := w.Engine.RemoveNetwork(cleanupCtx, netID, w.Gateway); e != nil {
				cleaned = false
				w.cleanupError(e)
			}
		}
		if ctx.Err() != nil {
			w.Interrupted.Add(1)
			w.StopMillis.Add(time.Since(start).Milliseconds())
		}
		var disposition struct {
			Status string  `json:"status"`
			Age    float64 `json:"since_transition_seconds"`
		}
		if e := w.Control.Do(cleanupCtx, "POST", "/internal/disposition", p, &disposition, nil); e == nil && cleaned && disposition.Status == "cancelled" {
			w.CancelCount.Add(1)
			w.CancelMillis.Add(int64(disposition.Age * 1000))
		}
	}
	defer cleanup()
	report := Report{TaskID: l.Task.ID, AttemptID: aid, Fence: p.Fence, Image: a.Profile.Image, Executor: a.Profile.Executor, Model: a.Profile.Model, Status: "failed", Verification: "not_verified", StartedAt: time.Now().UTC(), Tests: []domain.TestResult{}, Files: []string{}, TestsModified: []string{}, Note: "At-least-once execution; only this attempt's fenced result is authoritative. Test success is not proof of functional correctness. Unuploaded log tails can be lost on machine failure; task API retains attempt history."}
	log := &runner.Limited{Limit: 8 << 20}
	seq := int64(0)
	emit := func(kind, data string) {
		data = redact(data, l.Token)
		if log.Truncated && (kind == "agent.event" || kind == "agent") {
			return
		}
		_, _ = fmt.Fprintf(log, "[%s] %s\n", kind, data)
		if log.Truncated {
			report.LogTruncated = true
			data = "[LOG LIMIT REACHED; subsequent agent output discarded]"
		}
		for len(data) > 0 {
			n := min(len(data), 60000)
			chunk := data[:n]
			data = data[n:]
			seq++
			req := control.UpdateRequest{Proof: p, Events: []domain.Event{{Sequence: seq, Kind: kind, Data: chunk}}}
			var err error
			for i := 0; i < 3; i++ {
				err = w.Control.Do(ctx, "POST", "/internal/update", req, nil, nil)
				if err == nil || !pause(ctx, 200*time.Millisecond) {
					break
				}
			}
			if err != nil {
				report.Note += " Event persistence gap at sequence " + strconv.FormatInt(seq, 10) + "."
				slog.Warn("event persistence failed", "attempt", aid, "sequence", seq, "error", err)
			}
		}
	}
	stage := func(name string) error {
		emit("stage", name)
		return w.Control.Do(ctx, "POST", "/internal/update", control.UpdateRequest{Proof: p, Stage: name}, nil, nil)
	}
	exec := &ContainerExecutor{Runtime: w.Engine, Observe: func(line string) { emit("agent.event", line) }}
	var patch []byte
	retryable := false
	var runErr error
	runPipeline := func() error {
		var e error
		netID, e = w.Engine.Network(ctx, "dcar-"+aid, w.Gateway, labels)
		if e != nil {
			retryable = true
			return e
		}
		work, base, inputs := "dcar-work-"+aid, "dcar-base-"+aid, "dcar-inputs-"+aid
		for _, v := range []string{work, base, inputs} {
			if e = w.Engine.Volume(ctx, v, labels); e != nil {
				retryable = true
				return e
			}
			volumes = append(volumes, v)
		}
		proxy := "http://attempt:" + l.Token + "@gateway:8081"
		env := []string{"DCAR_ATTEMPT_TOKEN=" + l.Token, "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy, "NO_PROXY=gateway", "no_proxy=gateway", "HOME=/home/agent", "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1"}
		create := func(suffix string, mounts []docker.Mount) (string, error) {
			mounts = append(mounts, docker.Mount{Type: "volume", Source: inputs, Target: "/run/dcar"})
			id, e := w.Engine.Create(ctx, docker.Container{Image: a.Profile.Image, Name: "dcar-" + aid + "-" + suffix, Network: netID, Profile: a.Profile, Env: env, Command: []string{"/usr/local/bin/dcar-workspace", "supervise"}, Mounts: mounts, Labels: labels})
			if e != nil {
				return "", e
			}
			containers = append(containers, id)
			if e = w.Engine.Start(ctx, id); e != nil {
				return "", e
			}
			return id, nil
		}
		prep, e := create("prepare", []docker.Mount{{Type: "volume", Source: work, Target: "/workspace"}, {Type: "volume", Source: base, Target: "/baseline"}})
		if e != nil {
			retryable = true
			return e
		}
		var credential struct {
			Token string `json:"token"`
		}
		if e = w.Control.Do(ctx, "POST", "/internal/credential", p, &credential, nil); e != nil {
			retryable = true
			return e
		}
		var resolved runner.Prepared
		if e = exec.invoke(ctx, prep, "resolve", runner.Input{Repository: l.Task.Spec.Repository, Ref: l.Task.Spec.Ref, SHA: l.Task.SHA, Credential: credential.Token}, &resolved); e != nil {
			retryable = transient(e)
			return e
		}
		if e = w.Control.Do(ctx, "POST", "/internal/update", control.UpdateRequest{Proof: p, SHA: resolved.SHA}, nil, nil); e != nil {
			retryable = true
			return e
		}
		report.SHA = resolved.SHA
		prepared, e := exec.Prepare(ctx, prep, runner.Input{Repository: l.Task.Spec.Repository, Ref: l.Task.Spec.Ref, SHA: resolved.SHA, Credential: credential.Token, TestCommand: l.Task.Spec.TestCommand})
		credential.Token = ""
		if e != nil {
			retryable = transient(e)
			return e
		}
		report.SHA = prepared.SHA
		if e = w.Control.Do(ctx, "POST", "/internal/update", control.UpdateRequest{Proof: p, SHA: prepared.SHA}, nil, nil); e != nil {
			return e
		}
		if e = w.Engine.Stop(ctx, prep); e != nil {
			return e
		}
		if e = w.Engine.Remove(ctx, prep); e != nil {
			return e
		}
		containers = containers[1:]
		agent, e := create("agent", []docker.Mount{{Type: "volume", Source: work, Target: "/workspace"}})
		if e != nil {
			retryable = true
			return e
		}
		version, e := w.Engine.Exec(ctx, agent, []string{"codex", "--version"}, nil, 4096)
		if e == nil {
			report.AgentVersion = strings.TrimSpace(string(version.Output))
		}
		if a.Profile.Executor == "fixture" {
			report.AgentVersion = "fixture-v1"
		}
		if l.Task.Spec.PrepareCommand != "" {
			result, e := exec.Test(ctx, agent, runner.Input{Command: l.Task.Spec.PrepareCommand, Seconds: l.Task.Spec.TestTimeoutSeconds})
			emit("prepare", result.Output)
			if e != nil {
				return e
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("prepare_command_failed")
			}
		}
		if prepared.TestCommand != "" {
			result, e := exec.Test(ctx, agent, runner.Input{Command: prepared.TestCommand, Seconds: l.Task.Spec.TestTimeoutSeconds})
			if e != nil {
				return e
			}
			report.Baseline = &result
			emit("baseline_test", result.Output)
		}
		prompt := l.Task.Spec.Prompt
		var finalErr error
		for round := 0; round <= 2; round++ {
			if e = stage("agent"); e != nil {
				return e
			}
			result, e := exec.Execute(ctx, agent, runner.Input{Prompt: prompt, Model: a.Profile.Model, Executor: a.Profile.Executor})
			if e != nil {
				finalErr = e
				break
			}
			report.LogTruncated = report.LogTruncated || result.Truncated
			emit("agent", result.Output)
			if result.ExitCode != 0 || !result.Completed {
				retryable = result.ExitCode >= 128 || transient(fmt.Errorf("%s", result.Output))
				finalErr = fmt.Errorf("agent_failed")
				break
			}
			if prepared.TestCommand == "" {
				finalErr = fmt.Errorf("verification_unavailable: %s", prepared.VerificationError)
				break
			}
			if e = stage("testing"); e != nil {
				return e
			}
			test, e := exec.Test(ctx, agent, runner.Input{Command: prepared.TestCommand, Seconds: l.Task.Spec.TestTimeoutSeconds})
			if e != nil {
				finalErr = e
				break
			}
			report.Tests = append(report.Tests, test)
			report.RepairRounds = round
			emit("test", test.Output)
			if test.ExitCode == 0 && !test.TimedOut {
				report.Verification = "passed"
				w.TestPassed.Add(1)
				finalErr = nil
				break
			}
			w.TestFailed.Add(1)
			report.Verification = "failed"
			finalErr = fmt.Errorf("tests_failed")
			prompt = l.Task.Spec.Prompt + "\n\nThe platform's frozen verification command failed. Fix the code without weakening tests.\nCommand: " + prepared.TestCommand + "\nOutput:\n" + test.Output
			if a.Profile.Executor == "fixture" {
				prompt = l.Task.Spec.Prompt
			}
		}
		if e = stage("archiving"); e != nil {
			return e
		}
		if e = w.Engine.Stop(ctx, agent); e != nil {
			return e
		}
		if e = w.Engine.Remove(ctx, agent); e != nil {
			return e
		}
		containers = containers[:0]
		collector, e := create("collect", []docker.Mount{{Type: "volume", Source: work, Target: "/source", ReadOnly: true}, {Type: "volume", Source: base, Target: "/baseline", ReadOnly: true}})
		if e != nil {
			return e
		}
		var collected runner.Collected
		if e = exec.invoke(ctx, collector, "collect", runner.Input{SHA: report.SHA}, &collected); e != nil {
			return e
		}
		patch = collected.Patch
		report.Files = collected.Files
		report.TestsModified = collected.TestsModified
		report.ArtifactsComplete = true
		return finalErr
	}
	runErr = runPipeline()
	if ctx.Err() != nil {
		slog.Warn("attempt interrupted", "attempt", aid)
		return
	}
	if runErr != nil {
		if transient(runErr) {
			retryable = true
		}
		report.Error = redact(runErr.Error(), l.Token)
		emit("error", report.Error)
	} else {
		report.Status = "succeeded"
	}
	report.FinishedAt = time.Now().UTC()
	if e := w.Control.Do(ctx, "POST", "/internal/history", p, &report.Attempts, nil); e != nil {
		report.Note += " Attempt history could not be fetched; consult task API."
	} else {
		for i := range report.Attempts {
			if report.Attempts[i].ID == aid {
				report.Attempts[i].Status = report.Status
				report.Attempts[i].Error = report.Error
				report.Attempts[i].FinishedAt = &report.FinishedAt
			}
		}
	}
	report.LogTruncated = report.LogTruncated || log.Truncated
	// Reports and patch are immutable snapshots. A completion retry reuses this exact manifest and receipt key.
	b, _ := json.MarshalIndent(report, "", "  ")
	md := markdown(report)
	files := []struct {
		name string
		b    []byte
	}{{"report.json", b}, {"report.md", []byte(md)}, {"execution.log", log.Buffer.Bytes()}}
	if report.ArtifactsComplete {
		files = append(files, struct {
			name string
			b    []byte
		}{"changes.patch", patch})
	}
	manifest := []domain.Artifact{}
	for _, f := range files {
		var item domain.Artifact
		var e error
		for i := 0; i < 3; i++ {
			item, e = w.upload(ctx, p, f.name, f.b)
			if e == nil {
				break
			}
			if !pause(ctx, time.Duration(i+1)*time.Second) {
				return
			}
		}
		if e != nil {
			slog.Error("upload failed; lease recovery will retry attempt", "attempt", aid, "error", e)
			return
		}
		manifest = append(manifest, item)
	}
	verification := &domain.Verification{State: report.Verification, RepairRounds: report.RepairRounds, TestsModified: report.TestsModified, Tests: []domain.TestSummary{}}
	if report.Baseline != nil {
		baseline := domain.Summarize(*report.Baseline)
		verification.Baseline = &baseline
	}
	for _, test := range report.Tests {
		verification.Tests = append(verification.Tests, domain.Summarize(test))
	}
	completion := domain.Completion{Proof: p, Key: domain.ID(), Status: report.Status, Error: report.Error, Retryable: retryable, Artifacts: manifest, Verification: verification}
	for i := 0; i < 5; i++ {
		e := w.Control.Do(ctx, "POST", "/internal/complete", completion, nil, nil)
		if e == nil {
			w.Completed.Add(1)
			slog.Info("attempt committed", "attempt", aid, "status", report.Status)
			return
		}
		var he *client.Error
		if errors.As(e, &he) && he.Status < 500 {
			slog.Error("completion rejected", "attempt", aid, "error", e)
			return
		}
		if !pause(ctx, time.Duration(i+1)*time.Second) {
			return
		}
	}
	slog.Error("completion unconfirmed; durable receipt or lease reaper will resolve", "attempt", aid)
}
func transient(e error) bool {
	s := strings.ToLower(e.Error())
	for _, v := range []string{"timeout", "connection reset", "could not resolve", "couldn't connect", "connection refused", "rate limit", "rate_limit", "429", "502", "503", "504", "workspace process interrupted", "is not running", "no such exec instance", "unexpected eof", "remote hung up unexpectedly", "empty reply from server", "network is unreachable", "no route to host"} {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (w *Worker) upload(ctx context.Context, p domain.Proof, name string, b []byte) (domain.Artifact, error) {
	var a domain.Artifact
	r, e := http.NewRequestWithContext(ctx, "POST", w.Control.URL+"/internal/upload?name="+url.QueryEscape(name), bytes.NewReader(b))
	if e != nil {
		return a, e
	}
	raw, _ := json.Marshal(p)
	r.Header.Set("X-Proof", string(raw))
	r.Header.Set("Authorization", "Bearer "+w.Control.Token)
	res, e := w.Control.HTTP.Do(r)
	if e != nil {
		return a, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		out, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return a, &client.Error{Status: res.StatusCode, Body: string(out)}
	}
	e = json.NewDecoder(res.Body).Decode(&a)
	return a, e
}
func markdown(r Report) string {
	body := fmt.Sprintf("# Execution report\n\n- Task: `%s`\n- Attempt: `%s` (fence %d)\n- Baseline: `%s`\n- Status: **%s**\n- Verification: **%s**\n- Repair rounds: %d\n- Image: `%s`\n- Agent: `%s` / `%s`\n- Error: %s\n- Log truncated: %t\n- Artifacts complete: %t\n\n## Changed files\n\n```text\n%s\n```\n\n## Modified test/config files\n\n```text\n%s\n```\n\n%s\n", r.TaskID, r.AttemptID, r.Fence, r.SHA, r.Status, r.Verification, r.RepairRounds, r.Image, r.Executor, r.AgentVersion, r.Error, r.LogTruncated, r.ArtifactsComplete, strings.Join(r.Files, "\n"), strings.Join(r.TestsModified, "\n"), r.Note)
	var detail strings.Builder
	detail.WriteString("\n## Tests\n\n| Run | Exit | Duration (ms) | Timed out | Log truncated |\n|---|---:|---:|---|---|\n")
	row := func(label string, test domain.TestResult) {
		fmt.Fprintf(&detail, "| %s | %d | %d | %t | %t |\n", label, test.ExitCode, test.DurationMS, test.TimedOut, test.Truncated)
	}
	if r.Baseline != nil {
		row("Baseline", *r.Baseline)
	}
	for i, test := range r.Tests {
		row(fmt.Sprintf("Verification %d", i+1), test)
	}
	detail.WriteString("\nCommands and bounded output are recorded in report.json and execution.log.\n\n## Attempt history\n\n| Fence | Attempt | Worker | Result |\n|---:|---|---|---|\n")
	for _, a := range r.Attempts {
		fmt.Fprintf(&detail, "| %d | %s | %s | %s |\n", a.Fence, a.ID, a.WorkerID, a.Status)
	}
	return body + detail.String()
}
func (w *Worker) cleanupError(e error) {
	w.CleanupErrors.Add(1)
	slog.Error("resource cleanup failed; will retry", "error", e)
}
func (w *Worker) sweep(ctx context.Context) {
	ctx, c := context.WithTimeout(ctx, 30*time.Second)
	defer c()
	expired := func(labels map[string]string) bool {
		aid := labels["dcar.attempt"]
		w.mu.Lock()
		active := w.active[aid]
		w.mu.Unlock()
		if active {
			return false
		}
		f, _ := strconv.ParseInt(labels["dcar.fence"], 10, 64)
		p := domain.Proof{AttemptID: aid, WorkerID: labels["dcar.worker"], Session: labels["dcar.session"], Fence: f}
		e := w.Control.Do(ctx, "POST", "/internal/update", control.UpdateRequest{Proof: p}, nil, nil)
		var he *client.Error
		return errors.As(e, &he) && he.Status == 409
	}
	list, e := w.Engine.Containers(ctx, w.Registration.ID)
	if e != nil {
		w.cleanupError(e)
		return
	}
	for _, v := range list {
		if expired(v.Labels) {
			_ = w.Engine.Stop(ctx, v.ID)
			if e = w.Engine.Remove(ctx, v.ID); e != nil {
				w.cleanupError(e)
			}
		}
	}
	vols, e := w.Engine.Volumes(ctx, w.Registration.ID)
	if e == nil {
		for _, v := range vols {
			if expired(v.Labels) {
				if e = w.Engine.RemoveVolume(ctx, v.Name); e != nil {
					w.cleanupError(e)
				}
			}
		}
	} else {
		w.cleanupError(e)
	}
	nets, e := w.Engine.Networks(ctx, w.Registration.ID)
	if e == nil {
		for _, v := range nets {
			if expired(v.Labels) {
				if e = w.Engine.RemoveNetwork(ctx, v.ID, w.Gateway); e != nil {
					w.cleanupError(e)
				}
			}
		}
	} else {
		w.cleanupError(e)
	}
}
func (w *Worker) Metrics(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.mu.Lock()
	active := len(w.active)
	w.mu.Unlock()
	fmt.Fprintf(rw, "dcar_worker_active %d\ndcar_worker_available_capacity %d\n", active, max(0, w.Registration.Capacity-active))
	fmt.Fprintf(rw, "dcar_cleanup_failures_total %d\ndcar_worker_completed_total %d\ndcar_tests_total{result=\"passed\"} %d\ndcar_tests_total{result=\"failed\"} %d\n", w.CleanupErrors.Load(), w.Completed.Load(), w.TestPassed.Load(), w.TestFailed.Load())
	fmt.Fprintf(rw, "dcar_interrupted_cleanup_seconds_sum %g\ndcar_interrupted_cleanup_seconds_count %d\n", float64(w.StopMillis.Load())/1000, w.Interrupted.Load())
	fmt.Fprintf(rw, "dcar_cancel_reclaim_seconds_sum %g\ndcar_cancel_reclaim_seconds_count %d\n", float64(w.CancelMillis.Load())/1000, w.CancelCount.Load())
}
