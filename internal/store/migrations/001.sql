CREATE TABLE IF NOT EXISTS tasks (
 id text PRIMARY KEY, owner text NOT NULL, parent_id text NOT NULL DEFAULT '', spec jsonb NOT NULL,
 status text NOT NULL CHECK (status IN ('queued','running','retry_wait','succeeded','failed','cancelled','timed_out')),
 stage text NOT NULL DEFAULT '', sha text NOT NULL DEFAULT '', fence bigint NOT NULL DEFAULT 0,
 attempt_id text NOT NULL DEFAULT '', error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 deadline timestamptz NOT NULL, available_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS tasks_queue ON tasks(available_at,created_at) WHERE status IN ('queued','retry_wait');
CREATE TABLE IF NOT EXISTS workers (id text PRIMARY KEY, session text NOT NULL, capacity integer NOT NULL CHECK(capacity BETWEEN 1 AND 64), profiles text[] NOT NULL, updated_at timestamptz NOT NULL DEFAULT clock_timestamp());
CREATE TABLE IF NOT EXISTS attempts (
 id text PRIMARY KEY, task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, worker_id text NOT NULL, session text NOT NULL,
 fence bigint NOT NULL, status text NOT NULL, stage text NOT NULL DEFAULT '', error text NOT NULL DEFAULT '', token_hash text NOT NULL UNIQUE,
 lease_until timestamptz NOT NULL, started_at timestamptz NOT NULL DEFAULT clock_timestamp(), finished_at timestamptz,
 completion_key text, completion_hash text, UNIQUE(task_id,fence)
);
CREATE INDEX IF NOT EXISTS attempts_worker ON attempts(worker_id,session) WHERE status='running';
CREATE TABLE IF NOT EXISTS events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 attempt_id text NOT NULL, sequence bigint NOT NULL, kind text NOT NULL, data text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), UNIQUE(attempt_id,sequence)
);
CREATE INDEX IF NOT EXISTS events_task ON events(task_id,id);
CREATE TABLE IF NOT EXISTS artifacts (
 attempt_id text NOT NULL REFERENCES attempts(id) ON DELETE CASCADE, name text NOT NULL, key text NOT NULL UNIQUE,
 sha256 text NOT NULL, size bigint NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(attempt_id,name)
);
CREATE TABLE IF NOT EXISTS idempotency_records (
 owner text NOT NULL, key text NOT NULL, request_hash text NOT NULL, task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(owner,key)
);
