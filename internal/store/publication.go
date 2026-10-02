package store

import (
	"context"
	"errors"
	"github.com/dcar/runtime/internal/domain"
	"github.com/jackc/pgx/v5"
)

const pubCols = `task_id,status,branch,base,url,number,commit_sha,error,attempts,token,created_at`

func (s *Store) PublicationTask(ctx context.Context, p domain.Publication) (domain.Task, error) {
	return scanTask(s.Pool.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE id=$1 AND EXISTS(SELECT 1 FROM publications WHERE task_id=$1 AND token=$2 AND status='running' AND lease_until>clock_timestamp())", p.TaskID, p.Token))
}

func scanPublication(r pgx.Row) (p domain.Publication, e error) {
	e = r.Scan(&p.TaskID, &p.Status, &p.Branch, &p.Base, &p.URL, &p.Number, &p.Commit, &p.Error, &p.Attempts, &p.Token, &p.CreatedAt)
	return
}
func (s *Store) Publication(ctx context.Context, id string) (*domain.Publication, error) {
	p, e := scanPublication(s.Pool.QueryRow(ctx, "SELECT "+pubCols+" FROM publications WHERE task_id=$1", id))
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, e
}
func (s *Store) ClaimPublication(ctx context.Context) (*domain.Publication, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	// Recovery and expiry use database time. A crashed publisher is eligible after its lease.
	_, e = tx.Exec(ctx, `UPDATE publications SET status='failed',error='publication retry limit or retention deadline exceeded' WHERE status IN ('pending','running','retry_wait') AND (created_at<clock_timestamp()-interval '6 days' OR (attempts>=8 AND (lease_until IS NULL OR lease_until<clock_timestamp())))`)
	if e != nil {
		return nil, e
	}
	var id string
	e = tx.QueryRow(ctx, `SELECT task_id FROM publications WHERE (status IN ('pending','retry_wait') AND available_at<=clock_timestamp()) OR (status='running' AND lease_until<clock_timestamp()) ORDER BY available_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if e != nil {
		return nil, e
	}
	p, e := scanPublication(tx.QueryRow(ctx, `UPDATE publications SET status='running',attempts=attempts+1,token=$2,lease_until=clock_timestamp()+interval '120 seconds' WHERE task_id=$1 RETURNING `+pubCols, id, domain.ID()))
	if e != nil {
		return nil, e
	}
	return &p, tx.Commit(ctx)
}
func (s *Store) RenewPublication(ctx context.Context, p domain.Publication) error {
	tag, e := s.Pool.Exec(ctx, `UPDATE publications SET lease_until=clock_timestamp()+interval '120 seconds' WHERE task_id=$1 AND token=$2 AND status='running' AND lease_until>clock_timestamp()`, p.TaskID, p.Token)
	if e == nil && tag.RowsAffected() != 1 {
		return domain.ErrLease
	}
	return e
}
func (s *Store) FinishPublication(ctx context.Context, p domain.Publication, status string, retry bool) error {
	if status != "published" && status != "failed" && status != "no_changes" {
		return domain.ErrConflict
	}
	if retry && p.Attempts < 8 {
		status = "retry_wait"
	}
	tag, e := s.Pool.Exec(ctx, `UPDATE publications SET status=$3,url=$4,number=$5,commit_sha=$6,error=$7,lease_until=NULL,available_at=clock_timestamp()+make_interval(secs=>$8) WHERE task_id=$1 AND token=$2 AND status='running' AND lease_until>clock_timestamp()`, p.TaskID, p.Token, status, p.URL, p.Number, p.Commit, p.Error, backoff(int64(p.Attempts)))
	if e == nil && tag.RowsAffected() != 1 {
		return domain.ErrLease
	}
	return e
}

// Base is fixed before the first GitHub mutation, so default-branch changes cannot
// redirect a publication after a lost response or publisher restart.
func (s *Store) FixPublicationBase(ctx context.Context, p domain.Publication, base string) error {
	tag, e := s.Pool.Exec(ctx, `UPDATE publications SET base=$3 WHERE task_id=$1 AND token=$2 AND status='running' AND lease_until>clock_timestamp() AND (base='' OR base=$3)`, p.TaskID, p.Token, base)
	if e == nil && tag.RowsAffected() != 1 {
		return domain.ErrLease
	}
	return e
}
func (s *Store) RetryPublication(ctx context.Context, id string) (*domain.Publication, error) {
	_, e := s.Pool.Exec(ctx, `UPDATE publications SET status='pending',attempts=0,error='',available_at=clock_timestamp() WHERE task_id=$1 AND status='failed'`, id)
	if e != nil {
		return nil, e
	}
	p, e := s.Publication(ctx, id)
	if e == nil && p == nil {
		return nil, domain.ErrNotFound
	}
	return p, e
}
