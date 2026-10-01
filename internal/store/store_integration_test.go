package store

import (
	"context"
	"errors"
	"github.com/dcar/runtime/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"sync"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL unset: real PostgreSQL integration test not executed")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "dcar_test_" + domain.ID()
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 24
	p, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s := &Store{Pool: p, LeaseSeconds: 45}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close(); _, _ = admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); admin.Close() })
	return s
}
func spec() domain.Spec {
	s := domain.Spec{Repository: "https://github.com/example/repo", Prompt: "fix"}
	_ = s.Normalize()
	return s
}
func reg(t *testing.T, s *Store, id string) domain.Registration {
	t.Helper()
	r := domain.Registration{ID: id, Session: domain.ID(), Capacity: 4, Profiles: []string{"default"}}
	if e := s.Register(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	return r
}
func proofOf(l *domain.Lease) domain.Proof {
	return domain.Proof{AttemptID: l.Attempt.ID, WorkerID: l.Attempt.WorkerID, Session: l.Attempt.Session, Fence: l.Attempt.Fence}
}
func taskAndLease(t *testing.T, s *Store) (domain.Task, *domain.Lease) {
	t.Helper()
	v, e := s.Create(context.Background(), "owner", domain.ID(), spec(), "")
	if e != nil {
		t.Fatal(e)
	}
	r := reg(t, s, domain.ID())
	l, e := s.Claim(context.Background(), r)
	if e != nil || l == nil {
		t.Fatal(l, e)
	}
	return v, l
}
func TestConcurrentIdempotencyAndClaims(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.Create(ctx, "owner", "key", spec(), "")
			if e != nil {
				errs <- e
			} else {
				ids <- v.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	first := ""
	for id := range ids {
		if first != "" && id != first {
			t.Fatal("duplicate task")
		}
		first = id
	}
	changed := spec()
	changed.Prompt = "different"
	if _, e := s.Create(ctx, "owner", "key", changed, ""); !errors.Is(e, domain.ErrConflict) {
		t.Fatalf("expected conflict: %v", e)
	}
	workers := []domain.Registration{reg(t, s, "w1"), reg(t, s, "w2")}
	leases := make(chan *domain.Lease, 2)
	for _, r := range workers {
		wg.Add(1)
		go func(r domain.Registration) {
			defer wg.Done()
			l, e := s.Claim(ctx, r)
			if e != nil {
				t.Error(e)
			}
			leases <- l
		}(r)
	}
	wg.Wait()
	close(leases)
	count := 0
	for l := range leases {
		if l != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d claims", count)
	}
}
func TestLeaseLossSHAAndStaleWorker(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task, l := taskAndLease(t, s)
	p := proofOf(l)
	sha := strings.Repeat("a", 40)
	if _, e := s.Update(ctx, p, "", sha, nil, false); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE attempts SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", p.AttemptID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Update(ctx, p, "", "", nil, true); !errors.Is(e, domain.ErrLease) {
		t.Fatalf("expired lease revived: %v", e)
	}
	if _, e := s.Reap(ctx); e != nil {
		t.Fatal(e)
	}
	_, _ = s.Pool.Exec(ctx, "UPDATE tasks SET available_at=clock_timestamp() WHERE id=$1", task.ID)
	next, e := s.Claim(ctx, reg(t, s, "replacement"))
	if e != nil || next == nil {
		t.Fatal(next, e)
	}
	if next.Task.SHA != sha || next.Attempt.Fence != 2 {
		t.Fatal("lost SHA or fence")
	}
	if _, e = s.Update(ctx, proofOf(next), "", strings.Repeat("b", 40), nil, false); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("SHA overwritten")
	}
	if e = s.Complete(ctx, domain.Completion{Proof: p, Key: "old", Status: "succeeded"}); !errors.Is(e, domain.ErrLease) {
		t.Fatal("stale completion accepted", e)
	}
	if _, _, _, e = s.AuthorizeToken(ctx, l.Token); !errors.Is(e, domain.ErrLease) {
		t.Fatal("stale token accepted")
	}
}
func TestCompletionReceiptAndRetryHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task, l := taskAndLease(t, s)
	c := domain.Completion{Proof: proofOf(l), Key: "receipt", Status: "failed", Error: "tests_failed"}
	if e := s.Complete(ctx, c); e != nil {
		t.Fatal(e)
	}
	if e := s.Complete(ctx, c); e != nil {
		t.Fatal("lost response retry failed", e)
	}
	c.Status = "succeeded"
	if e := s.Complete(ctx, c); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("receipt changed", e)
	}
	v, e := s.Create(ctx, "owner", "retry", task.Spec, task.ID)
	if e != nil || v.ParentID != task.ID {
		t.Fatal(v, e)
	}
	old, e := s.Get(ctx, task.ID, "owner")
	if e != nil || old.Status != "failed" {
		t.Fatal("history modified")
	}
}
func TestCancelCompletionRace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		task, l := taskAndLease(t, s)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, e := s.Cancel(ctx, task.ID, "owner")
			if e != nil {
				t.Error(e)
			}
		}()
		go func() {
			defer wg.Done()
			e := s.Complete(ctx, domain.Completion{Proof: proofOf(l), Key: domain.ID(), Status: "succeeded"})
			if e != nil && !errors.Is(e, domain.ErrLease) {
				t.Error(e)
			}
		}()
		wg.Wait()
		v, e := s.Get(ctx, task.ID, "owner")
		if e != nil {
			t.Fatal(e)
		}
		if v.Status != "cancelled" && v.Status != "succeeded" {
			t.Fatal(v.Status)
		}
		_, _ = s.Reap(ctx)
		again, _ := s.Get(ctx, task.ID, "owner")
		if again.Status != v.Status {
			t.Fatal("terminal state changed")
		}
	}
}
func TestDeadlineAndEventReplay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task, l := taskAndLease(t, s)
	p := proofOf(l)
	ev := []domain.Event{{Sequence: 1, Kind: "log", Data: "hello"}}
	for i := 0; i < 2; i++ {
		if _, e := s.Update(ctx, p, "", "", ev, false); e != nil {
			t.Fatal(e)
		}
	}
	events, e := s.Events(ctx, task.ID, 0)
	if e != nil || len(events) != 1 {
		t.Fatal(events, e)
	}
	after, e := s.Events(ctx, task.ID, events[0].ID)
	if e != nil || len(after) != 0 {
		t.Fatal("replay repeated acknowledged event")
	}
	_, _ = s.Pool.Exec(ctx, "UPDATE tasks SET deadline=clock_timestamp()-interval '1 second' WHERE id=$1", task.ID)
	if _, e = s.Reap(ctx); e != nil {
		t.Fatal(e)
	}
	v, _ := s.Get(ctx, task.ID, "owner")
	if v.Status != "timed_out" {
		t.Fatal(v.Status)
	}
}
func TestRestartRevokesPreviousSessionAndCapacity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, e := s.Create(ctx, "owner", domain.ID(), spec(), ""); e != nil {
			t.Fatal(e)
		}
	}
	r := reg(t, s, "engine")
	r.Capacity = 1
	if e := s.Register(ctx, r); e != nil {
		t.Fatal(e)
	}
	l, e := s.Claim(ctx, r)
	if e != nil || l == nil {
		t.Fatal(e)
	}
	if extra, e := s.Claim(ctx, r); e != nil || extra != nil {
		t.Fatal("capacity exceeded")
	}
	r.Session = domain.ID()
	if e = s.Register(ctx, r); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Update(ctx, proofOf(l), "", "", nil, true); !errors.Is(e, domain.ErrLease) {
		t.Fatal("old session renewed")
	}
}

func TestRenewCancellationRace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		task, l := taskAndLease(t, s)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, e := s.Update(ctx, proofOf(l), "", "", nil, true)
			if e != nil && !errors.Is(e, domain.ErrLease) {
				t.Errorf("renew: %v", e)
			}
		}()
		go func() {
			defer wg.Done()
			if _, e := s.Cancel(ctx, task.ID, "owner"); e != nil {
				t.Errorf("cancel: %v", e)
			}
		}()
		wg.Wait()
		if _, e := s.Update(ctx, proofOf(l), "", "", nil, true); !errors.Is(e, domain.ErrLease) {
			t.Fatal("renew resurrected cancelled task")
		}
	}
}
