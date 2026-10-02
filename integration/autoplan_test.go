package integration

import (
	"context"
	"encoding/json"
	"github.com/dcar/runtime/internal/domain"
	"os"
	"strings"
	"testing"
)

// Uses the user's real original Vue repository and lockfile, without a model key.
// No prepare/test commands are submitted; the platform must discover both.
func TestLiveAutomaticVueBuild(t *testing.T) {
	if os.Getenv("E2E_AUTOPLAN") != "1" {
		t.Skip("E2E_AUTOPLAN=1 required for real dependency installation/build")
	}
	c := e2eClient(t)
	spec := domain.Spec{Repository: "https://github.com/ZoeySigel/vue2-todolist", Ref: "8e3f5da2b3b7f02241bfa71ff281606f63c22a75", Prompt: `{"path":"dcar-auto-check.txt","content":"automatic\n"}`, Profile: "fixture", TimeoutSeconds: 600, TestTimeoutSeconds: 240}
	var task domain.Task
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	t.Log("automatic task", task.ID)
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" {
		t.Fatalf("automatic build failed: %+v", final)
	}
	if final.Plan == nil || final.Plan.Source != "detected" || final.Plan.Kind != "build" || final.Plan.PrepareCommand != "npm ci --no-audit --no-fund" || final.Plan.TestCommand != "npm run build" {
		t.Fatalf("wrong automatic plan: %+v", final.Plan)
	}
	var report struct {
		Preparation, Baseline *domain.TestResult
		Tests                 []domain.TestResult
		Plan                  *domain.ExecutionPlan `json:"execution_plan"`
	}
	if e := json.Unmarshal(download(t, c, task.ID, "report.json"), &report); e != nil {
		t.Fatal(e)
	}
	if report.Preparation == nil || report.Preparation.ExitCode != 0 || report.Baseline == nil || report.Baseline.ExitCode != 0 || len(report.Tests) != 1 || report.Tests[0].ExitCode != 0 || report.Plan == nil || *report.Plan != *final.Plan {
		t.Fatalf("missing independent execution evidence: %+v", report)
	}
	if !strings.Contains(string(download(t, c, task.ID, "changes.patch")), "+automatic") {
		t.Fatal("fixture modification not archived")
	}
}

// A repository without supported manifests must use the real model planner.
// A README-only repository must honestly report missing verification, while
// still archiving the editing agent's result.
func TestLiveCodexAutomaticPlanner(t *testing.T) {
	if os.Getenv("E2E_CODEX") != "1" {
		t.Skip("E2E_CODEX=1 and model credentials required")
	}
	c := e2eClient(t)
	spec := domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: "Append one line 'Automatic planning smoke check.' to README. Do not change other files.", Profile: "default", TimeoutSeconds: 480, TestTimeoutSeconds: 180}
	var task domain.Task
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	t.Log("planner task", task.ID)
	final := waitTask(t, c, task.ID)
	if final.Plan == nil || final.Plan.Source != "agent" || final.Plan.SHA != final.SHA {
		t.Fatalf("missing agent-generated frozen plan: %+v", final)
	}
	if final.Plan.Kind == "unavailable" {
		if final.Status != "failed" || !strings.Contains(final.Error, "verification_unavailable") {
			t.Fatalf("unverifiable repository falsely passed: %+v", final)
		}
		t.Log("planner correctly reported unavailable verification for README-only repository")
	} else if final.Status != "succeeded" {
		t.Fatalf("automatic Codex task failed: %+v", final)
	}
	if !strings.Contains(string(download(t, c, task.ID, "changes.patch")), "+Automatic planning smoke check.") {
		t.Fatal("editing agent did not complete task")
	}
}

func TestLiveCodexAutomaticVue(t *testing.T) {
	if os.Getenv("E2E_CODEX") != "1" || os.Getenv("E2E_AUTOPLAN") != "1" {
		t.Skip("E2E_CODEX=1/E2E_AUTOPLAN=1 required")
	}
	c := e2eClient(t)
	spec := domain.Spec{Repository: "https://github.com/ZoeySigel/vue2-todolist", Ref: "8e3f5da2b3b7f02241bfa71ff281606f63c22a75", Prompt: "Append one line 'Automatic Vue smoke check.' to README.md. Do not change any other files or reinstall dependencies; the platform handles preparation and verification.", Profile: "default", TimeoutSeconds: 600, TestTimeoutSeconds: 240}
	var task domain.Task
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	t.Log("real automatic Vue task", task.ID)
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" || final.Plan == nil || final.Plan.Kind != "build" || final.Plan.Source != "detected" {
		t.Fatalf("real automatic Vue pipeline failed: %+v", final)
	}
	if !strings.Contains(string(download(t, c, task.ID, "changes.patch")), "+Automatic Vue smoke check.") {
		t.Fatal("real model edit not archived")
	}
}

func TestLiveCodexAutomaticShell(t *testing.T) {
	if os.Getenv("E2E_CODEX") != "1" || os.Getenv("E2E_PLANNER_SHELL") != "1" {
		t.Skip("E2E_CODEX=1/E2E_PLANNER_SHELL=1, model credits and compatible shell toolchain required")
	}
	c := e2eClient(t)
	spec := domain.Spec{Repository: "https://github.com/kward/shunit2", Ref: "f39734a3dd56495c2fee1d4b0925bb31082e36d2", Prompt: "Append one line 'Automatic planning smoke check.' at the end of README.md. Do not change other files.", Profile: "default", TimeoutSeconds: 480, TestTimeoutSeconds: 180}
	var task domain.Task
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	t.Log("real shell planner task", task.ID)
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" || final.Plan == nil || final.Plan.Source != "agent" || final.Plan.Kind == "unavailable" {
		t.Fatalf("real model-planned verification failed: %+v", final)
	}
	if !strings.Contains(string(download(t, c, task.ID, "changes.patch")), "+Automatic planning smoke check.") {
		t.Fatal("real model edit not archived")
	}
}
