package store

import (
	"context"
	"encoding/json"
	"github.com/dcar/runtime/internal/domain"
)

// FreezePlan shares the lease/fencing lock order with cancellation and completion.
// Repeated delivery is idempotent; a later attempt cannot replace the first plan.
func (s *Store) FreezePlan(ctx context.Context, proof domain.Proof, plan domain.ExecutionPlan) (domain.ExecutionPlan, error) {
	if e := plan.Validate(); e != nil {
		return plan, e
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return plan, e
	}
	defer tx.Rollback(ctx)
	task, e := s.lock(ctx, tx, proof)
	if e != nil {
		return plan, e
	}
	if task.SHA == "" || plan.SHA != task.SHA ||
		(task.Spec.TestCommand != "" && task.Spec.TestCommand != plan.TestCommand) ||
		(task.Spec.PrepareCommand != "" && task.Spec.PrepareCommand != plan.PrepareCommand) {
		return plan, domain.ErrConflict
	}
	if task.Plan != nil {
		if *task.Plan != plan {
			return plan, domain.ErrConflict
		}
		return *task.Plan, tx.Commit(ctx)
	}
	if task.Stage != "preparing" {
		return plan, domain.ErrConflict
	}
	raw, _ := json.Marshal(plan)
	if _, e = tx.Exec(ctx, "UPDATE tasks SET execution_plan=$2,updated_at=clock_timestamp() WHERE id=$1", task.ID, raw); e != nil {
		return plan, e
	}
	return plan, tx.Commit(ctx)
}
