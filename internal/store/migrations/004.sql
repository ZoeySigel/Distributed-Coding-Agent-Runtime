CREATE TABLE IF NOT EXISTS publications (
 task_id text PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
 status text NOT NULL CHECK(status IN ('pending','running','retry_wait','published','failed','no_changes')),
 branch text NOT NULL, base text NOT NULL DEFAULT '', url text NOT NULL DEFAULT '',
 number integer NOT NULL DEFAULT 0, commit_sha text NOT NULL DEFAULT '', error text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0, token text NOT NULL DEFAULT '', lease_until timestamptz,
 available_at timestamptz NOT NULL DEFAULT clock_timestamp(), created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS publications_queue ON publications(available_at,created_at) WHERE status IN ('pending','running','retry_wait');
