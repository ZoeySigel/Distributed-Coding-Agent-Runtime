package store

import (
	"context"
	"errors"
	"github.com/dcar/runtime/internal/domain"
	"strings"
	"testing"
)

func TestFrozenPlanSurvivesRecoveryAndCannotBeReplaced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	task, lease := taskAndLease(t, s)
	proof := proofOf(lease)
	sha := strings.Repeat("a", 40)
	if _, e := s.Update(ctx, proof, "", sha, nil, false); e != nil {
		t.Fatal(e)
	}
	plan := domain.ExecutionPlan{SHA: sha, Source: "detected", PrepareCommand: "npm ci", TestCommand: "npm run build", Kind: "build", Rationale: "Original build script."}
	for i := 0; i < 2; i++ {
		if got, e := s.FreezePlan(ctx, proof, plan); e != nil || got != plan {
			t.Fatal(got, e)
		}
	}
	other := plan
	other.TestCommand = "true"
	if _, e := s.FreezePlan(ctx, proof, other); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("plan replacement accepted", e)
	}
	_, e := s.Pool.Exec(ctx, "UPDATE attempts SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", proof.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.FreezePlan(ctx, proof, plan); !errors.Is(e, domain.ErrLease) {
		t.Fatal("expired worker changed plan", e)
	}
	if _, e = s.Reap(ctx); e != nil {
		t.Fatal(e)
	}
	_, e = s.Pool.Exec(ctx, "UPDATE tasks SET available_at=clock_timestamp() WHERE id=$1", task.ID)
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.Claim(ctx, reg(t, s, "recovery"))
	if e != nil || next == nil || next.Task.Plan == nil || *next.Task.Plan != plan {
		t.Fatal(next, e)
	}
	if _, e = s.FreezePlan(ctx, proof, plan); !errors.Is(e, domain.ErrLease) {
		t.Fatal("old worker regained plan rights", e)
	}
	if _, e = s.Cancel(ctx, task.ID, "owner"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.FreezePlan(ctx, proofOf(next), plan); !errors.Is(e, domain.ErrLease) {
		t.Fatal("cancelled worker changed plan", e)
	}
	retried, e := s.Create(ctx, "owner", domain.ID(), task.Spec, task.ID)
	if e != nil || retried.Plan == nil || *retried.Plan != plan || retried.SHA != sha {
		t.Fatal("manual retry lost plan", retried, e)
	}
}
