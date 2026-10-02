package control

import (
	"context"
	"encoding/json"
	"github.com/dcar/runtime/internal/domain"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicationAPIEnforcesOwnerAndHidesLeaseToken(t *testing.T) {
	db := controlDB(t)
	ctx := context.Background()
	spec := domain.Spec{Repository: "https://github.com/team/repo", Prompt: "fix"}
	_ = spec.Normalize()
	task, e := db.Create(ctx, "alice", domain.ID(), spec, "")
	if e != nil {
		t.Fatal(e)
	}
	reg := domain.Registration{ID: "publisher-api-worker", Session: "one", Capacity: 1, Profiles: []string{"default"}}
	if e := db.Register(ctx, reg); e != nil {
		t.Fatal(e)
	}
	l, e := db.Claim(ctx, reg)
	if e != nil {
		t.Fatal(e)
	}
	proof := domain.Proof{AttemptID: l.Attempt.ID, WorkerID: reg.ID, Session: reg.Session, Fence: l.Attempt.Fence}
	if e := db.Complete(ctx, domain.Completion{Proof: proof, Key: "pub-api", Status: "succeeded"}); e != nil {
		t.Fatal(e)
	}
	publication, e := db.ClaimPublication(ctx)
	if e != nil || publication == nil {
		t.Fatal(e)
	}
	handler := (&Server{DB: db, Tokens: map[string]string{"alice": "a", "bob": "b"}}).Handler()
	request := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, suffix := range []string{"/pr", "/pr/retry"} {
		method := "GET"
		if strings.HasSuffix(suffix, "retry") {
			method = "POST"
		}
		if w := request(method, "/v1/tasks/"+task.ID+suffix, "b"); w.Code != 404 {
			t.Fatal("publication owner boundary broken", w.Code)
		}
	}
	w := request("GET", "/v1/tasks/"+task.ID+"/pr", "a")
	if w.Code != 200 || strings.Contains(w.Body.String(), publication.Token) {
		t.Fatal("publication lease leaked", w.Code, w.Body.String())
	}
	if e := db.FinishPublication(ctx, *publication, "failed", false); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		w := request("POST", "/v1/tasks/"+task.ID+"/pr/retry", "a")
		var p domain.Publication
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		if w.Code != 202 || p.Status != "pending" {
			t.Fatal("publication retry not idempotent", w.Code, w.Body.String())
		}
	}
	got, e := db.Get(ctx, task.ID, "alice")
	if e != nil || got.Status != "succeeded" {
		t.Fatal("retry mutated terminal coding task", got, e)
	}
}
