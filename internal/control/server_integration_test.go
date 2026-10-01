package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

type faultObjects struct {
	mu          sync.Mutex
	items       map[string][]byte
	unavailable bool
}

func (o *faultObjects) Put(_ context.Context, id, name string, b []byte) (domain.Artifact, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unavailable {
		return domain.Artifact{}, errors.New("injected object outage")
	}
	a := domain.Artifact{Name: name, SHA256: store.Hash(b), Size: int64(len(b))}
	a.Key = id + "/" + a.SHA256 + "/" + name
	o.items[a.Key] = bytes.Clone(b)
	return a, nil
}
func (o *faultObjects) Verify(_ context.Context, a domain.Artifact) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unavailable {
		return errors.New("injected object outage")
	}
	b, ok := o.items[a.Key]
	if !ok || int64(len(b)) != a.Size || store.Hash(b) != a.SHA256 {
		return errors.New("missing object")
	}
	return nil
}
func (o *faultObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unavailable {
		return nil, errors.New("injected object outage")
	}
	b, ok := o.items[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func controlDB(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL unset: real API/database integration not executed")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := pgx.Identifier{"api_test_" + domain.ID()}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema[1 : len(schema)-1]
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	db := &store.Store{Pool: pool, LeaseSeconds: 45}
	if e = db.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close(); _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	return db
}
func TestAPIObjectFailureFencingAndReceipt(t *testing.T) {
	db := controlDB(t)
	ctx := context.Background()
	objects := &faultObjects{items: map[string][]byte{}}
	s := &Server{DB: db, Objects: objects, Tokens: map[string]string{"alice": "alice-token", "bob": "bob-token"}, WorkerToken: "worker", Profiles: map[string]domain.Profile{"default": {Model: "test"}}}
	handler := s.Handler()
	request := func(method, path, token string, v any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(v)
		r := httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", domain.ID())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	spec := domain.Spec{Repository: "https://github.com/example/repo", Prompt: "fix"}
	res := request("POST", "/v1/tasks", "alice-token", spec)
	if res.Code != 202 {
		t.Fatal(res.Code, res.Body.String())
	}
	var task domain.Task
	_ = json.Unmarshal(res.Body.Bytes(), &task)
	if got := request("GET", "/v1/tasks/"+task.ID, "bob-token", nil); got.Code != 404 {
		t.Fatal("owner isolation broken", got.Code)
	}
	if got := request("GET", "/v1/tasks", "bad", nil); got.Code != 401 {
		t.Fatal("auth broken")
	}
	reg := domain.Registration{ID: "worker", Session: "session", Capacity: 1, Profiles: []string{"default"}}
	if e := db.Register(ctx, reg); e != nil {
		t.Fatal(e)
	}
	l, e := db.Claim(ctx, reg)
	if e != nil || l == nil {
		t.Fatal(e)
	}
	p := domain.Proof{AttemptID: l.Attempt.ID, WorkerID: reg.ID, Session: reg.Session, Fence: l.Attempt.Fence}
	manifest := []domain.Artifact{}
	for _, name := range []string{"changes.patch", "report.json", "report.md", "execution.log"} {
		a, e := objects.Put(ctx, p.AttemptID, name, []byte("test"))
		if e != nil {
			t.Fatal(e)
		}
		manifest = append(manifest, a)
	}
	completion := domain.Completion{Proof: p, Key: "receipt", Status: "succeeded", Artifacts: manifest, Verification: &domain.Verification{State: "passed", Tests: []domain.TestSummary{{Command: "true", ExitCode: 0}}}}
	objects.unavailable = true
	if got := request("POST", "/internal/complete", "worker", completion); got.Code != 500 {
		t.Fatal(got.Code)
	}
	current, _ := db.Get(ctx, task.ID, "alice")
	if current.Status != "running" {
		t.Fatal("published before objects verified")
	}
	objects.unavailable = false
	invalid := completion
	invalid.Verification = nil
	if got := request("POST", "/internal/complete", "worker", invalid); got.Code != 400 {
		t.Fatal("unverified success accepted", got.Code)
	}
	if _, e := db.Pool.Exec(ctx, `CREATE FUNCTION reject_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='succeeded' THEN RAISE EXCEPTION 'injected database publication failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_publication BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION reject_publication()`); e != nil {
		t.Fatal(e)
	}
	if got := request("POST", "/internal/complete", "worker", completion); got.Code != 500 {
		t.Fatal("database fault not surfaced", got.Code)
	}
	if artifacts, e := db.Artifacts(ctx, task.ID); e != nil || len(artifacts) != 0 {
		t.Fatal("rolled-back manifest became visible", e)
	}
	if done, e := db.Receipt(ctx, completion); e != nil || done {
		t.Fatal("failed transaction created receipt", e)
	}
	if _, e := db.Pool.Exec(ctx, `DROP TRIGGER reject_publication ON tasks; DROP FUNCTION reject_publication()`); e != nil {
		t.Fatal(e)
	}
	if got := request("POST", "/internal/complete", "worker", completion); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	objects.unavailable = true
	if got := request("GET", "/v1/tasks/"+task.ID, "alice-token", nil); got.Code != 200 || !bytes.Contains(got.Body.Bytes(), []byte(`"verification"`)) {
		t.Fatal("test conclusion missing from database-backed detail", got.Code)
	}
	if got := request("POST", "/internal/complete", "worker", completion); got.Code != 200 {
		t.Fatal("receipt replay depended on object store", got.Code, got.Body.String())
	}
	objects.unavailable = false
	task2, e := db.Create(ctx, "alice", domain.ID(), task.Spec, "")
	if e != nil {
		t.Fatal(e)
	}
	l2, e := db.Claim(ctx, reg)
	if e != nil || l2 == nil {
		t.Fatal(e)
	}
	p2 := domain.Proof{AttemptID: l2.Attempt.ID, WorkerID: reg.ID, Session: reg.Session, Fence: l2.Attempt.Fence}
	orphan, _ := objects.Put(ctx, p2.AttemptID, "report.json", []byte("orphan"))
	if _, e = db.Cancel(ctx, task2.ID, "alice"); e != nil {
		t.Fatal(e)
	}
	if got := request("POST", "/internal/disposition", "worker", p2); got.Code != 200 || !bytes.Contains(got.Body.Bytes(), []byte(`"cancelled"`)) {
		t.Fatal("cleanup observation rejected historical proof", got.Code)
	}
	got := request("POST", "/internal/complete", "worker", domain.Completion{Proof: p2, Key: "stale", Status: "failed", Artifacts: []domain.Artifact{orphan}})
	if got.Code != 409 {
		t.Fatal("stale manifest published", got.Code)
	}
	artifacts, _ := db.Artifacts(ctx, task2.ID)
	if len(artifacts) != 0 {
		t.Fatal("orphan object became a result")
	}
}
