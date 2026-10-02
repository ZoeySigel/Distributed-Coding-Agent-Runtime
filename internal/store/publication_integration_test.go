package store

import (
	"context"
	"errors"
	"github.com/dcar/runtime/internal/domain"
	"sync"
	"testing"
)

func TestPublicationOutboxRecoveryAndFencing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task, l := taskAndLease(t, s)
	complete := domain.Completion{Proof: proofOf(l), Key: "publication-receipt", Status: "succeeded"}
	if e := s.Complete(ctx, complete); e != nil {
		t.Fatal(e)
	}
	if e := s.Complete(ctx, complete); e != nil {
		t.Fatal(e)
	}
	var count int
	if e := s.Pool.QueryRow(ctx, "SELECT count(*) FROM publications WHERE task_id=$1", task.ID).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winner *domain.Publication
	claims := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := s.ClaimPublication(ctx)
			if e != nil {
				t.Error(e)
			}
			if p != nil {
				mu.Lock()
				winner = p
				claims++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claims != 1 {
		t.Fatal("competing publishers claimed", claims)
	}
	if e := s.FixPublicationBase(ctx, *winner, "main"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.PublicationTask(ctx, *winner); e != nil {
		t.Fatal(e)
	}
	if e := s.FixPublicationBase(ctx, *winner, "other"); !errors.Is(e, domain.ErrLease) {
		t.Fatal("base changed", e)
	}
	_, e := s.Pool.Exec(ctx, "UPDATE publications SET lease_until=clock_timestamp()-interval '1 second' WHERE task_id=$1", task.ID)
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.ClaimPublication(ctx)
	if e != nil || next == nil || next.Token == winner.Token || next.Base != "main" || next.Attempts != 2 {
		t.Fatal(next, e)
	}
	if e := s.FinishPublication(ctx, *winner, "published", false); !errors.Is(e, domain.ErrLease) {
		t.Fatal("stale publisher accepted", e)
	}
	next.URL = "https://github.com/team/repo/pull/9"
	next.Number = 9
	if e := s.FinishPublication(ctx, *next, "published", false); e != nil {
		t.Fatal(e)
	}
	if got, e := s.RetryPublication(ctx, task.ID); e != nil || got.Status != "published" {
		t.Fatal("terminal result reset", got, e)
	}
	p, e := s.ClaimPublication(ctx)
	if e != nil || p != nil {
		t.Fatal(p, e)
	}
}
func TestPublicationOnlySuccessfulOptedInTasks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i, status := range []string{"failed", "succeeded","failed"} {
		spec := spec()
		no := false
		if i!=2{spec.AutoPR = &no}
		task, e := s.Create(ctx, "owner", domain.ID(), spec, "")
		if e != nil {
			t.Fatal(e)
		}
		l, e := s.Claim(ctx, reg(t, s, domain.ID()))
		if e != nil {
			t.Fatal(e)
		}
		if e := s.Complete(ctx, domain.Completion{Proof: proofOf(l), Key: domain.ID(), Status: status}); e != nil {
			t.Fatal(e)
		}
		if p, e := s.Publication(ctx, task.ID); e != nil || p != nil {
			t.Fatal(p, e)
		}
	}
}
