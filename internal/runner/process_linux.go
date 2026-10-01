//go:build linux

package runner

import (
	"context"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func process(ctx context.Context, cmd *exec.Cmd) (int, bool, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := cmd.Start(); e != nil {
		return -1, false, e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timed := false
	var e error
	select {
	case e = <-done:
	case <-ctx.Done():
		timed = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case e = <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			e = <-done
		}
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	code := 0
	if e != nil {
		code = 1
		if exit, ok := e.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
	}
	return code, timed, nil
}
func Shell(ctx context.Context, in Input) (domain.TestResult, error) {
	if in.Seconds < 1 {
		in.Seconds = 600
	}
	ctx, c := context.WithTimeout(ctx, time.Duration(in.Seconds)*time.Second)
	defer c()
	start := time.Now()
	cmd := exec.Command("/bin/sh", "-c", in.Command)
	cmd.Dir = "/workspace/repo"
	cmd.Env = os.Environ()
	out := &Limited{Limit: OutputLimit}
	cmd.Stdout = out
	cmd.Stderr = out
	code, timed, e := process(ctx, cmd)
	return domain.TestResult{Command: in.Command, ExitCode: code, DurationMS: time.Since(start).Milliseconds(), TimedOut: timed, Output: out.Buffer.String(), Truncated: out.Truncated}, e
}
func Agent(ctx context.Context, in Input) (AgentResult, error) {
	if in.Executor == "fixture" {
		return Fixture(ctx, in)
	}
	args := CodexArgs("http://gateway:8081/v1", in.Model, in.ModelCatalog)
	cmd := exec.Command("codex", args...)
	cmd.Dir = "/workspace/repo"
	cmd.Stdin = strings.NewReader(in.Prompt)
	cmd.Env = append(os.Environ(), "CODEX_HOME=/home/agent/.codex")
	out := &Limited{Limit: 8 << 20}
	parser := &AgentStream{Output: out}
	stream := io.MultiWriter(parser, os.Stdout)
	cmd.Stdout = stream
	cmd.Stderr = stream
	code, _, e := process(ctx, cmd)
	result := AgentResult{ExitCode: code, Output: out.Buffer.String(), Truncated: out.Truncated, Completed: parser.Completed}
	return result, e
}
