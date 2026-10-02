package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func e2eClient(t *testing.T) *client.Client {
	t.Helper()
	if os.Getenv("E2E_URL") == "" || os.Getenv("E2E_TOKEN") == "" {
		t.Skip("E2E_URL/E2E_TOKEN unset: live Docker pipeline not executed")
	}
	return client.New(os.Getenv("E2E_URL"), os.Getenv("E2E_TOKEN"))
}
func submit(t *testing.T, c *client.Client, prompt, test string) domain.Task {
	t.Helper()
	repo := os.Getenv("E2E_REPOSITORY")
	if repo == "" {
		repo = "https://github.com/octocat/Hello-World"
	}
	spec := domain.Spec{Repository: repo, Prompt: prompt, Profile: "fixture", TestCommand: test, TimeoutSeconds: 300, TestTimeoutSeconds: 15}
	var task domain.Task
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	return task
}
func waitTask(t *testing.T, c *client.Client, id string) domain.Task {
	t.Helper()
	until := time.Now().Add(320 * time.Second)
	deadlineLoaded := false
	for time.Now().Before(until) {
		var v struct {
			Task domain.Task `json:"task"`
		}
		if e := c.Do(context.Background(), "GET", "/v1/tasks/"+id, nil, &v, nil); e != nil {
			t.Fatal(e)
		}
		if domain.Terminal(v.Task.Status) {
			return v.Task
		}
		if !deadlineLoaded && !v.Task.Deadline.IsZero() {
			until = v.Task.Deadline.Add(20 * time.Second)
			deadlineLoaded = true
		}
		time.Sleep(time.Second)
	}
	t.Fatal("task did not terminate")
	return domain.Task{}
}
func download(t *testing.T, c *client.Client, id, name string) []byte {
	t.Helper()
	req, _ := http.NewRequest("GET", c.URL+"/v1/tasks/"+id+"/artifacts/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, e := c.HTTP.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != res.Header.Get("X-Checksum-Sha256") {
		t.Fatal("checksum mismatch")
	}
	return b
}
func TestFixtureSuccessAndArtifacts(t *testing.T) {
	c := e2eClient(t)
	task := submit(t, c, `{"path":"dcar-result.txt","content":"fixed\n"}`, `test "$(cat dcar-result.txt)" = fixed`)
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" {
		t.Fatalf("%+v", final)
	}
	patch := download(t, c, task.ID, "changes.patch")
	if !strings.Contains(string(patch), "+fixed") {
		t.Fatal("patch missing new file")
	}
	var report struct {
		Verification      string
		SHA               string
		ArtifactsComplete bool `json:"artifacts_complete"`
	}
	if e := json.Unmarshal(download(t, c, task.ID, "report.json"), &report); e != nil {
		t.Fatal(e)
	}
	if report.SHA != final.SHA || report.Verification != "passed" || !report.ArtifactsComplete {
		t.Fatalf("bad report %+v", report)
	}
}
func TestFixtureRepairLimitAndCancel(t *testing.T) {
	c := e2eClient(t)
	task := submit(t, c, `{"path":"dcar-result.txt","content":"wrong"}`, "exit 1")
	final := waitTask(t, c, task.ID)
	if final.Status != "failed" || final.Fence != 1 {
		t.Fatalf("test failure retried as infrastructure error: %+v", final)
	}
	var report struct {
		Rounds int                 `json:"repair_rounds"`
		Tests  []domain.TestResult `json:"tests"`
	}
	_ = json.Unmarshal(download(t, c, task.ID, "report.json"), &report)
	if report.Rounds != 2 || len(report.Tests) != 3 {
		t.Fatalf("repair limit violated %+v", report)
	}
	task = submit(t, c, `{"path":"late.txt","content":"late","delay_ms":120000}`, "true")
	waitStage(t, c, task.ID, "agent")
	if e := c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	final = waitTask(t, c, task.ID)
	if final.Status != "cancelled" {
		t.Fatal(final.Status)
	}
	if os.Getenv("E2E_DOCKER_FAILURES") == "1" {
		waitClean(t, final.AttemptID)
	}
}

func waitStage(t *testing.T, c *client.Client, id, stage string) domain.Task {
	t.Helper()
	until := time.Now().Add(90 * time.Second)
	for time.Now().Before(until) {
		var v struct {
			Task domain.Task `json:"task"`
		}
		if e := c.Do(context.Background(), "GET", "/v1/tasks/"+id, nil, &v, nil); e != nil {
			t.Fatal(e)
		}
		if v.Task.Status == "running" && v.Task.Stage == stage && v.Task.SHA != "" {
			return v.Task
		}
		if domain.Terminal(v.Task.Status) {
			t.Fatalf("task terminated before %s: %+v", stage, v.Task)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("stage not reached")
	return domain.Task{}
}

func dockerCommand(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if e != nil {
		t.Fatalf("docker %v: %s: %v", args, b, e)
	}
	return strings.TrimSpace(string(b))
}

func waitClean(t *testing.T, attempt string) {
	t.Helper()
	until := time.Now().Add(75 * time.Second)
	for time.Now().Before(until) {
		containers := dockerCommand(t, "ps", "-aq", "--filter", "label=dcar.attempt="+attempt)
		volumes := dockerCommand(t, "volume", "ls", "-q", "--filter", "label=dcar.attempt="+attempt)
		networks := dockerCommand(t, "network", "ls", "-q", "--filter", "label=dcar.attempt="+attempt)
		if containers == "" && volumes == "" && networks == "" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("attempt resources were not reclaimed")
}

func TestLiveWorkerCrashRecovery(t *testing.T) {
	if os.Getenv("E2E_DOCKER_FAILURES") != "1" {
		t.Skip("explicit E2E_DOCKER_FAILURES=1 required for disruptive dedicated-platform tests")
	}
	c := e2eClient(t)
	worker := os.Getenv("E2E_WORKER_CONTAINER")
	if worker == "" {
		worker = "dcar-worker-1"
	}
	task := submit(t, c, `{"path":"recovered.txt","content":"recovered","delay_ms":8000}`, "test -f recovered.txt")
	old := waitStage(t, c, task.ID, "agent")
	dockerCommand(t, "kill", "--signal", "KILL", worker)
	dockerCommand(t, "start", worker)
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" || final.Fence <= old.Fence || final.SHA != old.SHA {
		t.Fatalf("crash recovery violated fixed SHA or fencing: old=%+v final=%+v", old, final)
	}
	waitClean(t, old.AttemptID)
	_ = download(t, c, task.ID, "changes.patch")
}

func TestLiveWorkspaceFailureRecovery(t *testing.T) {
	if os.Getenv("E2E_DOCKER_FAILURES") != "1" {
		t.Skip("explicit E2E_DOCKER_FAILURES=1 required for disruptive dedicated-platform tests")
	}
	c := e2eClient(t)
	for _, failure := range []string{"container_exit", "network_partition"} {
		t.Run(failure, func(t *testing.T) {
			delay := 8000
			if failure == "network_partition" {
				delay = 50000
			}
			task := submit(t, c, fmt.Sprintf(`{"path":"recovered.txt","content":"recovered","delay_ms":%d}`, delay), "test -f recovered.txt")
			old := waitStage(t, c, task.ID, "agent")
			if failure == "container_exit" {
				dockerCommand(t, "kill", "dcar-"+old.AttemptID+"-agent")
			} else {
				gateway := os.Getenv("E2E_GATEWAY_CONTAINER")
				if gateway == "" {
					gateway = "dcar-gateway"
				}
				dockerCommand(t, "network", "disconnect", "dcar-"+old.AttemptID, gateway)
			}
			final := waitTask(t, c, task.ID)
			if final.Status != "succeeded" || final.Fence <= old.Fence || final.SHA != old.SHA {
				t.Fatalf("workspace fault recovery violated authority: old=%+v final=%+v", old, final)
			}
			waitClean(t, old.AttemptID)
		})
	}
}

func TestFixtureWorkspaceRestrictions(t *testing.T) {
	c := e2eClient(t)
	command := `test "$(id -u)" = 1000 && ! test -e /var/run/docker.sock && ! touch /etc/dcar-write && ! env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy wget -T 2 -O /dev/null http://169.254.169.254/latest/meta-data/ && test -f restricted.txt`
	task := submit(t, c, `{"path":"restricted.txt","content":"ok"}`, command)
	if final := waitTask(t, c, task.ID); final.Status != "succeeded" {
		t.Fatalf("workspace restrictions failed: %+v", final)
	}
}

func TestLiveObjectStorageOutage(t *testing.T) {
	if os.Getenv("E2E_DOCKER_FAILURES") != "1" {
		t.Skip("dedicated platform required for object storage outage")
	}
	c := e2eClient(t)
	storage := os.Getenv("E2E_STORAGE_CONTAINER")
	if storage == "" {
		storage = "dcar-minio-1"
	}
	task := submit(t, c, `{"path":"durable.txt","content":"stored","delay_ms":8000}`, "test -f durable.txt")
	waitStage(t, c, task.ID, "agent")
	dockerCommand(t, "stop", "--time", "2", storage)
	t.Cleanup(func() { dockerCommand(t, "start", storage) })
	waitStage(t, c, task.ID, "archiving")
	time.Sleep(3 * time.Second)
	var v struct {
		Task domain.Task `json:"task"`
	}
	if e := c.Do(context.Background(), "GET", "/v1/tasks/"+task.ID, nil, &v, nil); e != nil {
		t.Fatal(e)
	}
	if v.Task.Status == "succeeded" {
		t.Fatal("success published while storage unavailable")
	}
	dockerCommand(t, "start", storage)
	if final := waitTask(t, c, task.ID); final.Status != "succeeded" {
		t.Fatalf("storage recovery failed: %+v", final)
	}
	_ = download(t, c, task.ID, "changes.patch")
}

func TestFixtureTotalDeadline(t *testing.T) {
	c := e2eClient(t)
	var task domain.Task
	spec := domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: `{"path":"late.txt","content":"late","delay_ms":50000}`, Profile: "fixture", TestCommand: "true", TimeoutSeconds: 12, TestTimeoutSeconds: 1}
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	final := waitTask(t, c, task.ID)
	if final.Status != "timed_out" {
		t.Fatalf("deadline not enforced: %+v", final)
	}
	if os.Getenv("E2E_DOCKER_FAILURES") == "1" && final.AttemptID != "" {
		waitClean(t, final.AttemptID)
	}
}

func TestFixtureGoToolchain(t *testing.T) {
	c := e2eClient(t)
	var task domain.Task
	prompt, _ := json.Marshal(map[string]string{"path": "runtime_test.go", "content": "package fixture\nimport \"testing\"\nfunc TestFixture(t *testing.T) {}\n"})
	spec := domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: string(prompt), Profile: "fixture", PrepareCommand: `printf 'module fixture\n\ngo 1.24\n' > go.mod`, TestCommand: "go test ./...", TimeoutSeconds: 300, TestTimeoutSeconds: 90}
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	if final := waitTask(t, c, task.ID); final.Status != "succeeded" {
		t.Fatalf("real Go tests could not execute: %+v", final)
	}
	var report struct {
		Baseline *domain.TestResult  `json:"baseline"`
		Tests    []domain.TestResult `json:"tests"`
		Modified []string            `json:"tests_modified"`
	}
	if e := json.Unmarshal(download(t, c, task.ID, "report.json"), &report); e != nil {
		t.Fatal(e)
	}
	if report.Baseline == nil || report.Baseline.ExitCode == 0 || len(report.Tests) != 1 || report.Tests[0].ExitCode != 0 || len(report.Modified) == 0 {
		t.Fatalf("real Go test evidence missing: %+v", report)
	}
}

func TestFixtureVerificationUnavailableAndTestTimeout(t *testing.T) {
	c := e2eClient(t)
	task := submit(t, c, `{"path":"unverified.txt","content":"change"}`, "")
	final := waitTask(t, c, task.ID)
	if final.Status != "failed" || !strings.Contains(final.Error, "verification_unavailable") {
		t.Fatalf("unverified task incorrectly accepted: %+v", final)
	}
	_ = download(t, c, task.ID, "changes.patch")
	task = submit(t, c, `{"path":"timeout.txt","content":"change"}`, "sleep 30")
	final = waitTask(t, c, task.ID)
	if final.Status != "failed" || final.Fence != 1 {
		t.Fatalf("test timeout incorrectly retried: %+v", final)
	}
	var report struct {
		Tests    []domain.TestResult `json:"tests"`
		Baseline *domain.TestResult  `json:"baseline"`
	}
	if e := json.Unmarshal(download(t, c, task.ID, "report.json"), &report); e != nil {
		t.Fatal(e)
	}
	if report.Baseline == nil || !report.Baseline.TimedOut || len(report.Tests) != 3 {
		t.Fatalf("incomplete timeout report: %+v", report)
	}
	for _, test := range report.Tests {
		if !test.TimedOut {
			t.Fatal("timeout not recorded")
		}
	}
}
func TestLiveCodexSmoke(t *testing.T) {
	if os.Getenv("E2E_CODEX") != "1" {
		t.Skip("E2E_CODEX=1 and configured model credentials required; real Codex not executed")
	}
	c := e2eClient(t)
	var task domain.Task
	spec := domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: "Create a file dcar-smoke.txt containing exactly the word ready followed by a newline. Do not change other files.", Profile: "default", TestCommand: `test "$(cat dcar-smoke.txt)" = ready`, TimeoutSeconds: 300, TestTimeoutSeconds: 15}
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" {
		t.Fatal(fmt.Sprintf("live Codex failed: %+v", final))
	}
	_ = download(t, c, task.ID, "changes.patch")
}
