package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcar/runtime/internal/artifact"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/store"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Server struct {
	DB          *store.Store
	Objects     artifact.ArtifactStore
	Tokens      map[string]string
	WorkerToken string
	Profiles    map[string]domain.Profile
	Credentials map[string]string
	ClaimCalls  atomic.Int64
	ClaimNanos  atomic.Int64
	ClaimEmpty  atomic.Int64
	ClaimErrors atomic.Int64
}

func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, e error) {
	code := 500
	msg := "internal server error"
	switch {
	case errors.Is(e, domain.ErrNotFound):
		code = 404
		msg = e.Error()
	case errors.Is(e, domain.ErrConflict):
		code = 409
		msg = e.Error()
	case errors.Is(e, domain.ErrLease):
		code = 409
		msg = e.Error()
	default:
		slog.Error("request failed", "error", e)
	}
	JSON(w, code, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		JSON(w, 400, map[string]string{"error": "invalid JSON: " + e.Error()})
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		JSON(w, 400, map[string]string{"error": "one JSON value required"})
		return false
	}
	return true
}
func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, c := context.WithTimeout(r.Context(), 2*time.Second)
		defer c()
		if e := s.DB.Pool.Ping(ctx); e != nil {
			JSON(w, 503, map[string]string{"status": "database unavailable"})
			return
		}
		JSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/v1/", s.public)
	mux.HandleFunc("/internal/", func(w http.ResponseWriter, r *http.Request) {
		if !equal(bearer(r), s.WorkerToken) {
			JSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		s.internal(w, r)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if !equal(bearer(r), s.WorkerToken) {
			w.WriteHeader(401)
			return
		}
		s.metrics(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) public(w http.ResponseWriter, r *http.Request) {
	owner := ""
	for name, token := range s.Tokens {
		if equal(bearer(r), token) {
			owner = name
			break
		}
	}
	if owner == "" {
		JSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[1] != "tasks" {
		w.WriteHeader(404)
		return
	}
	if len(parts) == 2 {
		switch r.Method {
		case "POST":
			var spec domain.Spec
			if !decode(w, r, &spec) {
				return
			}
			if e := spec.Normalize(); e != nil {
				JSON(w, 400, map[string]string{"error": e.Error()})
				return
			}
			if _, ok := s.Profiles[spec.Profile]; !ok {
				JSON(w, 400, map[string]string{"error": "unknown profile"})
				return
			}
			if spec.CredentialRef != "" {
				if _, ok := s.Credentials[spec.CredentialRef]; !ok {
					JSON(w, 400, map[string]string{"error": "unknown credential reference"})
					return
				}
			}
			if len(r.Header.Get("Idempotency-Key")) == 0 || len(r.Header.Get("Idempotency-Key")) > 200 {
				JSON(w, 400, map[string]string{"error": "Idempotency-Key required"})
				return
			}
			t, e := s.DB.Create(r.Context(), owner, r.Header.Get("Idempotency-Key"), spec, "")
			if e != nil {
				fail(w, e)
				return
			}
			JSON(w, 202, t)
		case "GET":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit < 1 || limit > 100 {
				limit = 50
			}
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			if offset < 0 {
				offset = 0
			}
			v, e := s.DB.List(r.Context(), owner, limit, offset)
			if e != nil {
				fail(w, e)
				return
			}
			JSON(w, 200, v)
		default:
			w.WriteHeader(405)
		}
		return
	}
	t, e := s.DB.Get(r.Context(), parts[2], owner)
	if e != nil {
		fail(w, e)
		return
	}
	if len(parts) == 3 && r.Method == "GET" {
		a, e := s.DB.Attempts(r.Context(), t.ID)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]any{"task": t, "attempts": a})
		return
	}
	if len(parts) < 4 {
		w.WriteHeader(405)
		return
	}
	if parts[3] == "report" && len(parts) == 4 && r.Method == "GET" {
		a, e := s.DB.Attempts(r.Context(), t.ID)
		if e != nil {
			fail(w, e)
			return
		}
		artifacts, e := s.DB.Artifacts(r.Context(), t.ID)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]any{"task": t, "attempts": a, "artifacts": artifacts, "note": "Control-plane audit report. Workspace details, if available, are in report.json. Cancellation, timeout and worker loss may leave no workspace snapshot; persisted events survive."})
		return
	}
	switch {
	case parts[3] == "cancel" && len(parts) == 4 && r.Method == "POST":
		v, e := s.DB.Cancel(r.Context(), t.ID, owner)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, v)
	case parts[3] == "retry" && len(parts) == 4 && r.Method == "POST":
		if len(r.Header.Get("Idempotency-Key")) == 0 || len(r.Header.Get("Idempotency-Key")) > 200 {
			JSON(w, 400, map[string]string{"error": "Idempotency-Key required"})
			return
		}
		v, e := s.DB.Create(r.Context(), owner, r.Header.Get("Idempotency-Key"), t.Spec, t.ID)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 202, v)
	case parts[3] == "events" && len(parts) == 4 && r.Method == "GET":
		s.events(w, r, t)
	case parts[3] == "artifacts" && r.Method == "GET":
		list, e := s.DB.Artifacts(r.Context(), t.ID)
		if e != nil {
			fail(w, e)
			return
		}
		if len(parts) == 4 {
			JSON(w, 200, list)
			return
		}
		if len(parts) != 5 {
			w.WriteHeader(404)
			return
		}
		for _, a := range list {
			if a.Name == parts[4] {
				reader, e := s.Objects.Get(r.Context(), a.Key)
				if e != nil {
					fail(w, e)
					return
				}
				defer reader.Close()
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", `attachment; filename="`+a.Name+`"`)
				w.Header().Set("X-Checksum-Sha256", a.SHA256)
				_, _ = io.Copy(w, reader)
				return
			}
		}
		w.WriteHeader(404)
	default:
		w.WriteHeader(404)
	}
}
func (s *Server) events(w http.ResponseWriter, r *http.Request, t domain.Task) {
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if after == 0 {
		after, _ = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	f, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(500)
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		events, e := s.DB.Events(r.Context(), t.ID, after)
		if e != nil {
			return
		}
		for _, v := range events {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "id: %d\nevent: execution\ndata: %s\n\n", v.ID, b)
			after = v.ID
		}
		v, e := s.DB.Get(r.Context(), t.ID, t.Owner)
		if e != nil {
			return
		}
		if domain.Terminal(v.Status) && len(events) < 200 {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: terminal\ndata: %s\n\n", b)
			f.Flush()
			return
		}
		fmt.Fprint(w, ": heartbeat\n\n")
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

type UpdateRequest struct {
	Proof  domain.Proof   `json:"proof"`
	Stage  string         `json:"stage,omitempty"`
	SHA    string         `json:"sha,omitempty"`
	Events []domain.Event `json:"events,omitempty"`
	Renew  bool           `json:"renew"`
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	ctx := r.Context()
	switch r.URL.Path {
	case "/internal/disposition":
		var p domain.Proof
		if !decode(w, r, &p) {
			return
		}
		var status string
		var age float64
		if e := s.DB.Pool.QueryRow(ctx, `SELECT t.status,EXTRACT(EPOCH FROM (clock_timestamp()-t.updated_at)) FROM attempts a JOIN tasks t ON t.id=a.task_id WHERE a.id=$1 AND a.worker_id=$2 AND a.session=$3 AND a.fence=$4`, p.AttemptID, p.WorkerID, p.Session, p.Fence).Scan(&status, &age); e != nil {
			fail(w, domain.ErrNotFound)
			return
		}
		JSON(w, 200, map[string]any{"status": status, "since_transition_seconds": age})
	case "/internal/history":
		var p domain.Proof
		if !decode(w, r, &p) {
			return
		}
		if _, e := s.DB.Update(ctx, p, "", "", nil, false); e != nil {
			fail(w, e)
			return
		}
		var tid string
		if e := s.DB.Pool.QueryRow(ctx, "SELECT task_id FROM attempts WHERE id=$1", p.AttemptID).Scan(&tid); e != nil {
			fail(w, e)
			return
		}
		v, e := s.DB.Attempts(ctx, tid)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, v)
	case "/internal/register":
		var v domain.Registration
		if !decode(w, r, &v) {
			return
		}
		for _, p := range v.Profiles {
			if _, ok := s.Profiles[p]; !ok {
				JSON(w, 400, map[string]string{"error": "unknown profile"})
				return
			}
		}
		if e := s.DB.Register(ctx, v); e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]string{"status": "registered"})
	case "/internal/claim":
		started := time.Now()
		defer func() { s.ClaimCalls.Add(1); s.ClaimNanos.Add(time.Since(started).Nanoseconds()) }()
		var v domain.Registration
		if !decode(w, r, &v) {
			return
		}
		l, e := s.DB.Claim(ctx, v)
		if e != nil {
			s.ClaimErrors.Add(1)
			fail(w, e)
			return
		}
		if l == nil {
			s.ClaimEmpty.Add(1)
			w.WriteHeader(204)
			return
		}
		JSON(w, 200, map[string]any{"lease": l, "profile": s.Profiles[l.Task.Spec.Profile]})
	case "/internal/update":
		var v UpdateRequest
		if !decode(w, r, &v) {
			return
		}
		if len(v.Events) > 100 || (v.SHA != "" && !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(v.SHA)) {
			JSON(w, 400, map[string]string{"error": "invalid update"})
			return
		}
		switch v.Stage {
		case "", "preparing", "agent", "testing", "archiving":
		default:
			JSON(w, 400, map[string]string{"error": "invalid stage"})
			return
		}
		until, e := s.DB.Update(ctx, v.Proof, v.Stage, v.SHA, v.Events, v.Renew)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]any{"lease_until": until, "ttl_seconds": s.DB.LeaseSeconds})
	case "/internal/credential":
		var p domain.Proof
		if !decode(w, r, &p) {
			return
		}
		if _, e := s.DB.Update(ctx, p, "", "", nil, false); e != nil {
			fail(w, e)
			return
		}
		var ref string
		if e := s.DB.Pool.QueryRow(ctx, "SELECT COALESCE(spec->>'credential_ref','') FROM tasks WHERE attempt_id=$1", p.AttemptID).Scan(&ref); e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]string{"token": s.Credentials[ref]})
	case "/internal/lease":
		var v struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &v) {
			return
		}
		id, p, until, e := s.DB.AuthorizeToken(ctx, v.Token)
		if e != nil {
			fail(w, e)
			return
		}
		var remaining float64
		if e = s.DB.Pool.QueryRow(ctx, "SELECT EXTRACT(EPOCH FROM ($1::timestamptz-clock_timestamp()))", until).Scan(&remaining); e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]any{"attempt_id": id, "model": s.Profiles[p].Model, "remaining_seconds": remaining})
	case "/internal/upload":
		var p domain.Proof
		if e := json.Unmarshal([]byte(r.Header.Get("X-Proof")), &p); e != nil {
			w.WriteHeader(400)
			return
		}
		if _, e := s.DB.Update(ctx, p, "", "", nil, false); e != nil {
			fail(w, e)
			return
		}
		name := r.URL.Query().Get("name")
		if !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`).MatchString(name) {
			w.WriteHeader(400)
			return
		}
		b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, artifact.MaxSize))
		if e != nil {
			w.WriteHeader(413)
			return
		}
		a, e := s.Objects.Put(ctx, p.AttemptID, name, b)
		if e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, a)
	case "/internal/complete":
		var c domain.Completion
		if !decode(w, r, &c) {
			return
		}
		if c.Key == "" || (c.Status != "succeeded" && c.Status != "failed") {
			JSON(w, 400, map[string]string{"error": "invalid completion"})
			return
		}
		if done, e := s.DB.Receipt(ctx, c); e != nil {
			fail(w, e)
			return
		} else if done {
			JSON(w, 200, map[string]string{"status": "committed"})
			return
		}
		if _, e := s.DB.Update(ctx, c.Proof, "", "", nil, false); e != nil {
			fail(w, e)
			return
		}
		seen := map[string]bool{}
		if len(c.Artifacts) > 20 {
			w.WriteHeader(400)
			return
		}
		for _, a := range c.Artifacts {
			if seen[a.Name] || !regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`).MatchString(a.Name) || a.Key != c.Proof.AttemptID+"/"+a.SHA256+"/"+a.Name || a.Size < 0 || a.Size > artifact.MaxSize {
				w.WriteHeader(400)
				return
			}
			seen[a.Name] = true
			if e := s.Objects.Verify(ctx, a); e != nil {
				fail(w, e)
				return
			}
		}
		if c.Status == "succeeded" && (!seen["changes.patch"] || !seen["report.json"] || !seen["report.md"] || !seen["execution.log"]) {
			JSON(w, 400, map[string]string{"error": "required artifacts missing"})
			return
		}
		if c.Status == "succeeded" {
			v := c.Verification
			if v == nil || v.State != "passed" || len(v.Tests) == 0 || v.Tests[len(v.Tests)-1].ExitCode != 0 || v.Tests[len(v.Tests)-1].TimedOut {
				JSON(w, 400, map[string]string{"error": "successful verification required"})
				return
			}
		}
		if e := s.DB.Complete(ctx, c); e != nil {
			fail(w, e)
			return
		}
		JSON(w, 200, map[string]string{"status": "committed"})
	default:
		w.WriteHeader(404)
	}
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "dcar_claim_seconds_sum %g\ndcar_claim_seconds_count %d\ndcar_claim_empty_total %d\ndcar_claim_errors_total %d\n", float64(s.ClaimNanos.Load())/1e9, s.ClaimCalls.Load(), s.ClaimEmpty.Load(), s.ClaimErrors.Load())
	stats := s.DB.Pool.Stat()
	fmt.Fprintf(w, "dcar_database_connections %d\ndcar_database_acquired_connections %d\ndcar_database_acquire_seconds_sum %g\ndcar_database_waits_total %d\n", stats.TotalConns(), stats.AcquiredConns(), stats.AcquireDuration().Seconds(), stats.EmptyAcquireCount())
	rows, e := s.DB.Pool.Query(r.Context(), "SELECT status,count(*) FROM tasks GROUP BY status")
	if e != nil {
		w.WriteHeader(503)
		return
	}
	for rows.Next() {
		var status string
		var n int
		_ = rows.Scan(&status, &n)
		fmt.Fprintf(w, "dcar_tasks{status=%q} %d\n", status, n)
	}
	rows.Close()
	for name, q := range map[string]string{"queue_oldest_seconds": "SELECT COALESCE(EXTRACT(EPOCH FROM (clock_timestamp()-min(created_at))),0) FROM tasks WHERE status IN ('queued','retry_wait')", "attempts_total": "SELECT count(*) FROM attempts", "retries_total": "SELECT count(*) FROM attempts WHERE fence>1", "lease_expirations_total": "SELECT count(*) FROM attempts WHERE error='worker_lost'", "execution_seconds_sum": "SELECT COALESCE(sum(EXTRACT(EPOCH FROM (finished_at-started_at))),0) FROM attempts WHERE finished_at IS NOT NULL"} {
		var n float64
		if e := s.DB.Pool.QueryRow(r.Context(), q).Scan(&n); e == nil {
			fmt.Fprintf(w, "dcar_%s %g\n", name, n)
		}
	}
}
