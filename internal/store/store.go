package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool         *pgxpool.Pool
	LeaseSeconds int
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	p, e := pgxpool.New(ctx, dsn)
	if e != nil {
		return nil, e
	}
	if e = p.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return &Store{p, 45}, nil
}
func (s *Store) Migrate(ctx context.Context) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(84123871)"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())"); e != nil {
		return e
	}
	entries, e := migrations.ReadDir("migrations")
	if e != nil {
		return e
	}
	for _, entry := range entries {
		b, e := migrations.ReadFile("migrations/" + entry.Name())
		if e != nil {
			return e
		}
		var prior string
		e = tx.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE name=$1", entry.Name()).Scan(&prior)
		if e == nil {
			if prior != Hash(b) {
				return fmt.Errorf("migration checksum changed: %s", entry.Name())
			}
			continue
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if _, e = tx.Exec(ctx, string(b)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(name,checksum) VALUES($1,$2)", entry.Name(), Hash(b)); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

const taskCols = `id,owner,parent_id,spec,status,stage,sha,fence,attempt_id,error,created_at,deadline,updated_at`

func scanTask(row pgx.Row) (t domain.Task, e error) {
	var b []byte
	e = row.Scan(&t.ID, &t.Owner, &t.ParentID, &b, &t.Status, &t.Stage, &t.SHA, &t.Fence, &t.AttemptID, &t.Error, &t.CreatedAt, &t.Deadline, &t.UpdatedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		e = domain.ErrNotFound
	}
	if e == nil {
		e = json.Unmarshal(b, &t.Spec)
	}
	return
}

const attemptCols = `id,task_id,worker_id,session,fence,status,stage,error,lease_until,started_at,finished_at,verification`

func scanAttempt(row pgx.Row) (a domain.Attempt, e error) {
	var b []byte
	e = row.Scan(&a.ID, &a.TaskID, &a.WorkerID, &a.Session, &a.Fence, &a.Status, &a.Stage, &a.Error, &a.LeaseUntil, &a.StartedAt, &a.FinishedAt, &b)
	if e == nil && b != nil {
		e = json.Unmarshal(b, &a.Verification)
	}
	return
}
func (s *Store) Get(ctx context.Context, id, owner string) (domain.Task, error) {
	return scanTask(s.Pool.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE id=$1 AND owner=$2", id, owner))
}
func (s *Store) List(ctx context.Context, owner string, limit, offset int) ([]domain.Task, error) {
	rows, e := s.Pool.Query(ctx, "SELECT "+taskCols+" FROM tasks WHERE owner=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3", owner, limit, offset)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) Create(ctx context.Context, owner, key string, spec domain.Spec, parent string) (domain.Task, error) {
	if key == "" || len(key) > 200 {
		return domain.Task{}, fmt.Errorf("idempotency key required (max 200 bytes)")
	}
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return domain.Task{}, e
	}
	defer tx.Rollback(ctx)
	// Serialize only requests sharing an idempotency scope, including retry requests.
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", owner+":"+key); e != nil {
		return domain.Task{}, e
	}
	b, _ := json.Marshal(struct {
		Spec   domain.Spec
		Parent string
	}{spec, parent})
	hash := Hash(b)
	var prior, ph string
	e = tx.QueryRow(ctx, "SELECT task_id,request_hash FROM idempotency_records WHERE owner=$1 AND key=$2", owner, key).Scan(&prior, &ph)
	if e == nil {
		if ph != hash {
			return domain.Task{}, domain.ErrConflict
		}
		return scanTask(tx.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE id=$1", prior))
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return domain.Task{}, e
	}
	sha := ""
	if parent != "" {
		p, e := scanTask(tx.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE id=$1 AND owner=$2 FOR UPDATE", parent, owner))
		if e != nil {
			return domain.Task{}, e
		}
		if p.Status != "failed" && p.Status != "cancelled" && p.Status != "timed_out" {
			return domain.Task{}, domain.ErrConflict
		}
		sha = p.SHA
	}
	raw, _ := json.Marshal(spec)
	id := domain.ID()
	t, e := scanTask(tx.QueryRow(ctx, "INSERT INTO tasks(id,owner,parent_id,spec,status,sha,deadline) VALUES($1,$2,$3,$4,'queued',$5,clock_timestamp()+make_interval(secs => $6)) RETURNING "+taskCols, id, owner, parent, raw, sha, spec.TimeoutSeconds))
	if e != nil {
		return t, e
	}
	_, e = tx.Exec(ctx, "INSERT INTO idempotency_records(owner,key,request_hash,task_id) VALUES($1,$2,$3,$4)", owner, key, hash, id)
	if e != nil {
		return t, e
	}
	return t, tx.Commit(ctx)
}
func (s *Store) Register(ctx context.Context, r domain.Registration) error {
	if r.ID == "" || r.Session == "" || r.Capacity < 1 || r.Capacity > 64 || len(r.Profiles) == 0 {
		return fmt.Errorf("invalid worker registration")
	}
	_, e := s.Pool.Exec(ctx, `INSERT INTO workers(id,session,capacity,profiles) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET session=excluded.session,capacity=excluded.capacity,profiles=excluded.profiles,updated_at=clock_timestamp()`, r.ID, r.Session, r.Capacity, r.Profiles)
	return e
}
func (s *Store) Claim(ctx context.Context, r domain.Registration) (*domain.Lease, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	var cap int
	var profiles []string
	if e = tx.QueryRow(ctx, "SELECT capacity,profiles FROM workers WHERE id=$1 AND session=$2 FOR UPDATE", r.ID, r.Session).Scan(&cap, &profiles); e != nil {
		return nil, domain.ErrLease
	}
	var n int
	if e = tx.QueryRow(ctx, "SELECT count(*) FROM attempts WHERE worker_id=$1 AND status='running' AND lease_until>clock_timestamp()", r.ID).Scan(&n); e != nil {
		return nil, e
	}
	if n >= cap {
		return nil, nil
	}
	t, e := scanTask(tx.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE status IN ('queued','retry_wait') AND available_at<=clock_timestamp() AND deadline>clock_timestamp() AND spec->>'profile'=ANY($1) ORDER BY available_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1", profiles))
	if errors.Is(e, domain.ErrNotFound) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	aid, token := domain.ID(), domain.ID()+domain.ID()
	t.Fence++
	t.AttemptID = aid
	t.Status = "running"
	t.Stage = "preparing"
	_, e = tx.Exec(ctx, "UPDATE tasks SET status='running',stage='preparing',fence=$2,attempt_id=$3,updated_at=clock_timestamp() WHERE id=$1", t.ID, t.Fence, aid)
	if e != nil {
		return nil, e
	}
	a, e := scanAttempt(tx.QueryRow(ctx, "INSERT INTO attempts(id,task_id,worker_id,session,fence,status,stage,token_hash,lease_until) VALUES($1,$2,$3,$4,$5,'running','preparing',$6,LEAST(clock_timestamp()+make_interval(secs => $7),$8)) RETURNING "+attemptCols, aid, t.ID, r.ID, r.Session, t.Fence, Hash([]byte(token)), s.LeaseSeconds, t.Deadline))
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return &domain.Lease{Task: t, Attempt: a, Token: token, TTLSeconds: s.LeaseSeconds}, nil
}

// Every execution mutation locks the task first. Cancellation, reaping and completion share this order.
func (s *Store) lock(ctx context.Context, tx pgx.Tx, p domain.Proof) (domain.Task, error) {
	t, e := scanTask(tx.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE attempt_id=$1 FOR UPDATE", p.AttemptID))
	if e != nil {
		return t, domain.ErrLease
	}
	var ok bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attempts a JOIN workers w ON w.id=a.worker_id AND w.session=a.session WHERE a.id=$1 AND a.worker_id=$2 AND a.session=$3 AND a.fence=$4 AND a.status='running' AND a.lease_until>clock_timestamp()) AND $5='running' AND $6>clock_timestamp()`, p.AttemptID, p.WorkerID, p.Session, p.Fence, t.Status, t.Deadline).Scan(&ok)
	if e != nil {
		return t, e
	}
	if !ok {
		return t, domain.ErrLease
	}
	return t, nil
}
func (s *Store) Update(ctx context.Context, p domain.Proof, stage, sha string, events []domain.Event, renew bool) (time.Time, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return time.Time{}, e
	}
	defer tx.Rollback(ctx)
	t, e := s.lock(ctx, tx, p)
	if e != nil {
		return time.Time{}, e
	}
	if sha != "" && t.SHA != "" && t.SHA != sha {
		return time.Time{}, domain.ErrConflict
	}
	if sha != "" {
		if _, e = tx.Exec(ctx, "UPDATE tasks SET sha=$2 WHERE id=$1 AND sha=''", t.ID, sha); e != nil {
			return time.Time{}, e
		}
	}
	if stage != "" {
		if _, e = tx.Exec(ctx, "UPDATE tasks SET stage=$2,updated_at=clock_timestamp() WHERE id=$1", t.ID, stage); e != nil {
			return time.Time{}, e
		}
		if _, e = tx.Exec(ctx, "UPDATE attempts SET stage=$2 WHERE id=$1", p.AttemptID, stage); e != nil {
			return time.Time{}, e
		}
	}
	for _, v := range events {
		if v.Sequence < 1 || len(v.Data) > 65536 {
			return time.Time{}, fmt.Errorf("invalid event")
		}
		var eventID int64
		e = tx.QueryRow(ctx, "INSERT INTO events(task_id,attempt_id,sequence,kind,data) VALUES($1,$2,$3,$4,$5) ON CONFLICT(attempt_id,sequence) DO UPDATE SET data=events.data WHERE events.kind=excluded.kind AND events.data=excluded.data RETURNING id", t.ID, p.AttemptID, v.Sequence, v.Kind, v.Data).Scan(&eventID)
		if errors.Is(e, pgx.ErrNoRows) {
			return time.Time{}, domain.ErrConflict
		}
		if e != nil {
			return time.Time{}, e
		}
	}
	var until time.Time
	if renew {
		e = tx.QueryRow(ctx, "UPDATE attempts SET lease_until=LEAST(clock_timestamp()+make_interval(secs => $2),$3) WHERE id=$1 RETURNING lease_until", p.AttemptID, s.LeaseSeconds, t.Deadline).Scan(&until)
	} else {
		e = tx.QueryRow(ctx, "SELECT lease_until FROM attempts WHERE id=$1", p.AttemptID).Scan(&until)
	}
	if e != nil {
		return until, e
	}
	return until, tx.Commit(ctx)
}
func (s *Store) Cancel(ctx context.Context, id, owner string) (domain.Task, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return domain.Task{}, e
	}
	defer tx.Rollback(ctx)
	t, e := scanTask(tx.QueryRow(ctx, "SELECT "+taskCols+" FROM tasks WHERE id=$1 AND owner=$2 FOR UPDATE", id, owner))
	if e != nil {
		return t, e
	}
	if domain.Terminal(t.Status) {
		return t, nil
	}
	_, e = tx.Exec(ctx, "UPDATE attempts SET status='cancelled',finished_at=clock_timestamp(),lease_until=clock_timestamp() WHERE id=$1 AND status='running'", t.AttemptID)
	if e != nil {
		return t, e
	}
	t, e = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET status='cancelled',error='cancelled by user',updated_at=clock_timestamp() WHERE id=$1 RETURNING "+taskCols, id))
	if e != nil {
		return t, e
	}
	return t, tx.Commit(ctx)
}
func (s *Store) Complete(ctx context.Context, c domain.Completion) error {
	if c.Key == "" || (c.Status != "succeeded" && c.Status != "failed") {
		return fmt.Errorf("invalid completion")
	}
	raw, _ := json.Marshal(c)
	hash := Hash(raw)
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Read stable identity, then acquire task lock before inspecting completion receipt.
	var tid string
	if e = tx.QueryRow(ctx, "SELECT task_id FROM attempts WHERE id=$1", c.Proof.AttemptID).Scan(&tid); e != nil {
		return domain.ErrLease
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM tasks WHERE id=$1 FOR UPDATE", tid); e != nil {
		return e
	}
	var oldKey, oldHash *string
	if e = tx.QueryRow(ctx, "SELECT completion_key,completion_hash FROM attempts WHERE id=$1", c.Proof.AttemptID).Scan(&oldKey, &oldHash); e != nil {
		return e
	}
	if oldKey != nil {
		if *oldKey == c.Key && oldHash != nil && *oldHash == hash {
			return nil
		}
		return domain.ErrConflict
	}
	t, e := s.lock(ctx, tx, c.Proof)
	if e != nil {
		return e
	}
	for _, a := range c.Artifacts {
		_, e = tx.Exec(ctx, "INSERT INTO artifacts(attempt_id,name,key,sha256,size) VALUES($1,$2,$3,$4,$5)", c.Proof.AttemptID, a.Name, a.Key, a.SHA256, a.Size)
		if e != nil {
			return e
		}
	}
	verification, _ := json.Marshal(c.Verification)
	_, e = tx.Exec(ctx, "UPDATE attempts SET status=$2,error=$3,finished_at=clock_timestamp(),completion_key=$4,completion_hash=$5,lease_until=clock_timestamp(),verification=$6 WHERE id=$1", c.Proof.AttemptID, c.Status, c.Error, c.Key, hash, verification)
	if e != nil {
		return e
	}
	status := c.Status
	if c.Status == "failed" && c.Retryable && t.Fence < 3 {
		status = "retry_wait"
	}
	_, e = tx.Exec(ctx, "UPDATE tasks SET status=$2,error=$3,stage='finished',updated_at=clock_timestamp(),available_at=clock_timestamp()+make_interval(secs => $4) WHERE id=$1", t.ID, status, c.Error, backoff(t.Fence))
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Receipt lookup precedes object-store access, so an acknowledged commit remains replayable during an S3 outage.
func (s *Store) Receipt(ctx context.Context, c domain.Completion) (bool, error) {
	var key, hash *string
	e := s.Pool.QueryRow(ctx, "SELECT completion_key,completion_hash FROM attempts WHERE id=$1 AND worker_id=$2 AND session=$3 AND fence=$4", c.Proof.AttemptID, c.Proof.WorkerID, c.Proof.Session, c.Proof.Fence).Scan(&key, &hash)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, domain.ErrLease
	}
	if e != nil {
		return false, e
	}
	if key == nil {
		return false, nil
	}
	b, _ := json.Marshal(c)
	if *key == c.Key && hash != nil && *hash == Hash(b) {
		return true, nil
	}
	return false, domain.ErrConflict
}
func backoff(f int64) float64 {
	b := float64(int64(5) * (int64(1) << uint(min(f, 5))))
	return b + float64(time.Now().UnixNano()%5000)/1000
}
func (s *Store) Reap(ctx context.Context) (int, error) {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT "+taskCols+" FROM tasks t WHERE status IN ('queued','running','retry_wait') AND (deadline<=clock_timestamp() OR (status='running' AND EXISTS(SELECT 1 FROM attempts a WHERE a.id=t.attempt_id AND a.lease_until<=clock_timestamp()))) ORDER BY deadline FOR UPDATE SKIP LOCKED LIMIT 100")
	if e != nil {
		return 0, e
	}
	var tasks []domain.Task
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			rows.Close()
			return 0, e
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return 0, e
	}
	for _, t := range tasks {
		var expired bool
		if e = tx.QueryRow(ctx, "SELECT $1::timestamptz<=clock_timestamp()", t.Deadline).Scan(&expired); e != nil {
			return 0, e
		}
		status, reason := "retry_wait", "worker_lost"
		if expired {
			status, reason = "timed_out", "deadline_exceeded"
		} else if t.Fence >= 3 {
			status = "failed"
		}
		if _, e = tx.Exec(ctx, "UPDATE attempts SET status=$2,error=$3,finished_at=clock_timestamp(),lease_until=clock_timestamp() WHERE id=$1 AND status='running'", t.AttemptID, "failed", reason); e != nil {
			return 0, e
		}
		if _, e = tx.Exec(ctx, "UPDATE tasks SET status=$2,error=$3,updated_at=clock_timestamp(),available_at=clock_timestamp()+make_interval(secs => $4) WHERE id=$1", t.ID, status, reason, backoff(t.Fence)); e != nil {
			return 0, e
		}
	}
	return len(tasks), tx.Commit(ctx)
}
func (s *Store) AuthorizeToken(ctx context.Context, token string) (string, string, time.Time, error) {
	var aid, model string
	var until time.Time
	e := s.Pool.QueryRow(ctx, `SELECT a.id,t.spec->>'profile',LEAST(a.lease_until,t.deadline) FROM attempts a JOIN tasks t ON t.attempt_id=a.id JOIN workers w ON w.id=a.worker_id AND w.session=a.session WHERE a.token_hash=$1 AND a.status='running' AND t.status='running' AND a.lease_until>clock_timestamp() AND t.deadline>clock_timestamp()`, Hash([]byte(token))).Scan(&aid, &model, &until)
	if e != nil {
		return "", "", until, domain.ErrLease
	}
	return aid, model, until, nil
}
func (s *Store) Attempts(ctx context.Context, id string) ([]domain.Attempt, error) {
	rows, e := s.Pool.Query(ctx, "SELECT "+attemptCols+" FROM attempts WHERE task_id=$1 ORDER BY fence", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Attempt{}
	for rows.Next() {
		a, e := scanAttempt(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) Events(ctx context.Context, id string, after int64) ([]domain.Event, error) {
	rows, e := s.Pool.Query(ctx, "SELECT id,attempt_id,sequence,kind,data,created_at FROM events WHERE task_id=$1 AND id>$2 ORDER BY id LIMIT 200", id, after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Event{}
	for rows.Next() {
		var v domain.Event
		if e = rows.Scan(&v.ID, &v.AttemptID, &v.Sequence, &v.Kind, &v.Data, &v.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Artifacts(ctx context.Context, id string) ([]domain.Artifact, error) {
	rows, e := s.Pool.Query(ctx, "SELECT r.name,r.key,r.sha256,r.size FROM artifacts r JOIN tasks t ON t.attempt_id=r.attempt_id WHERE t.id=$1 ORDER BY r.name", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var a domain.Artifact
		if e = rows.Scan(&a.Name, &a.Key, &a.SHA256, &a.Size); e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
