package integration

import (
	"context"
	"encoding/json"
	"github.com/dcar/runtime/internal/domain"
	"os"
	"strings"
	"testing"
	"time"
)

// This exercises the real Docker -> collector -> S3 -> outbox -> publisher path
// without a GitHub write token. It does not prove real PR creation.
func TestFixturePublicationWithoutWriteAccess(t *testing.T) {
	if os.Getenv("E2E_PUBLICATION_NO_CREDENTIAL") != "1" {
		t.Skip("E2E_PUBLICATION_NO_CREDENTIAL=1 and live publisher required")
	}
	c := e2eClient(t)
	spec := domain.Spec{Repository: "https://github.com/octocat/Hello-World", Prompt: `{"path":"dcar-publication.txt","content":"published snapshot\n"}`, Profile: "fixture", TestCommand: "test -f dcar-publication.txt", TimeoutSeconds: 180, TestTimeoutSeconds: 15}
	var task domain.Task
	if e := c.Do(context.Background(), "POST", "/v1/tasks", spec, &task, map[string]string{"Idempotency-Key": domain.ID()}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Do(context.Background(), "POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil, nil) })
	t.Log("publication task", task.ID)
	final := waitTask(t, c, task.ID)
	if final.Status != "succeeded" {
		t.Fatal(final)
	}
	var changes domain.ChangeSet
	if e := json.Unmarshal(download(t, c, task.ID, "publication.json"), &changes); e != nil {
		t.Fatal(e)
	}
	if e := changes.Validate(); e != nil {
		t.Fatal(e)
	}
	if changes.SHA != final.SHA || len(changes.Changes) != 1 || string(changes.Changes[0].Content) != "published snapshot\n" {
		t.Fatal(changes)
	}
	until := time.Now().Add(45 * time.Second)
	for time.Now().Before(until) {
		var p *domain.Publication
		if e := c.Do(context.Background(), "GET", "/v1/tasks/"+task.ID+"/pr", nil, &p, nil); e != nil {
			t.Fatal(e)
		}
		if p != nil && p.Status == "failed" {
			if !strings.Contains(p.Error, "not configured") {
				t.Fatal(p)
			}
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("publisher did not record missing repository write configuration")
}
