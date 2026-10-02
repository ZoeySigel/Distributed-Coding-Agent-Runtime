package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var ErrConflict = errors.New("conflict")
var ErrNotFound = errors.New("not found")
var ErrLease = errors.New("lease revoked or expired")

type Spec struct {
	Repository         string `json:"repository"`
	Prompt             string `json:"prompt"`
	Ref                string `json:"ref,omitempty"`
	CredentialRef      string `json:"credential_ref,omitempty"`
	Profile            string `json:"profile"`
	PrepareCommand     string `json:"prepare_command,omitempty"`
	TestCommand        string `json:"test_command,omitempty"`
	TimeoutSeconds     int    `json:"timeout_seconds"`
	TestTimeoutSeconds int    `json:"test_timeout_seconds"`
}

func (s *Spec) Normalize() error {
	u, e := url.Parse(s.Repository)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return fmt.Errorf("repository must be a GitHub HTTPS URL without credentials, query or fragment")
	}
	p := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	if !regexp.MustCompile(`^/[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$`).MatchString(p) {
		return fmt.Errorf("invalid repository path")
	}
	s.Repository = "https://github.com" + p + ".git"
	if strings.TrimSpace(s.Prompt) == "" || len(s.Prompt) > 65536 {
		return fmt.Errorf("prompt must contain 1..65536 bytes")
	}
	if strings.HasPrefix(s.Ref, "-") || strings.ContainsAny(s.Ref, "\x00\n\r ~^:?*[\\") || len(s.Ref) > 256 {
		return fmt.Errorf("invalid ref")
	}
	if s.Profile == "" {
		s.Profile = "default"
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 3600
	}
	if s.TestTimeoutSeconds == 0 {
		s.TestTimeoutSeconds = min(600, s.TimeoutSeconds)
	}
	if s.TimeoutSeconds < 10 || s.TimeoutSeconds > 86400 || s.TestTimeoutSeconds < 1 || s.TestTimeoutSeconds > s.TimeoutSeconds {
		return fmt.Errorf("invalid timeout")
	}
	if len(s.TestCommand) > 8192 || len(s.PrepareCommand) > 8192 {
		return fmt.Errorf("command too long")
	}
	return nil
}
func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Terminal(s string) bool {
	switch s {
	case "succeeded", "failed", "cancelled", "timed_out":
		return true
	}
	return false
}

type Task struct {
	ID        string         `json:"id"`
	Owner     string         `json:"-"`
	ParentID  string         `json:"parent_id,omitempty"`
	Spec      Spec           `json:"spec"`
	Status    string         `json:"status"`
	Stage     string         `json:"stage"`
	SHA       string         `json:"sha,omitempty"`
	Fence     int64          `json:"fence"`
	AttemptID string         `json:"attempt_id,omitempty"`
	Error     string         `json:"error,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	Deadline  time.Time      `json:"deadline"`
	UpdatedAt time.Time      `json:"updated_at"`
	Plan      *ExecutionPlan `json:"execution_plan,omitempty"`
}
type Attempt struct {
	ID           string        `json:"id"`
	TaskID       string        `json:"task_id"`
	WorkerID     string        `json:"worker_id"`
	Session      string        `json:"session"`
	Fence        int64         `json:"fence"`
	Status       string        `json:"status"`
	Stage        string        `json:"stage"`
	Error        string        `json:"error,omitempty"`
	LeaseUntil   time.Time     `json:"lease_until"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
	Verification *Verification `json:"verification,omitempty"`
}
type Lease struct {
	Task       Task    `json:"task"`
	Attempt    Attempt `json:"attempt"`
	Token      string  `json:"token"`
	TTLSeconds int     `json:"ttl_seconds"`
}
type Proof struct {
	AttemptID string `json:"attempt_id"`
	WorkerID  string `json:"worker_id"`
	Session   string `json:"session"`
	Fence     int64  `json:"fence"`
}
type Registration struct {
	ID       string   `json:"id"`
	Session  string   `json:"session"`
	Capacity int      `json:"capacity"`
	Profiles []string `json:"profiles"`
}
type Event struct {
	ID        int64     `json:"id"`
	AttemptID string    `json:"attempt_id"`
	Sequence  int64     `json:"sequence"`
	Kind      string    `json:"kind"`
	Data      string    `json:"data"`
	CreatedAt time.Time `json:"created_at"`
}
type Artifact struct {
	Name   string `json:"name"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Completion struct {
	Proof        Proof         `json:"proof"`
	Key          string        `json:"key"`
	Status       string        `json:"status"`
	Error        string        `json:"error,omitempty"`
	Retryable    bool          `json:"retryable"`
	Artifacts    []Artifact    `json:"artifacts"`
	Verification *Verification `json:"verification,omitempty"`
}

// TestSummary keeps API queries independent of log/object storage availability.
type TestSummary struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
}
type Verification struct {
	State         string        `json:"state"`
	Kind          string        `json:"kind,omitempty"`
	Baseline      *TestSummary  `json:"baseline,omitempty"`
	Tests         []TestSummary `json:"tests"`
	RepairRounds  int           `json:"repair_rounds"`
	TestsModified []string      `json:"tests_modified"`
}

func Summarize(t TestResult) TestSummary {
	return TestSummary{Command: t.Command, ExitCode: t.ExitCode, DurationMS: t.DurationMS, TimedOut: t.TimedOut, Truncated: t.Truncated}
}

type TestResult struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
}
type Profile struct {
	Image        string `json:"image"`
	Executor     string `json:"executor"`
	Model        string `json:"model"`
	ModelCatalog string `json:"model_catalog,omitempty"`
	CPU          int64  `json:"cpu"`
	Memory       int64  `json:"memory"`
	PIDs         int64  `json:"pids"`
}
