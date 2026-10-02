package publisher

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGitHubLostResponsesReplayWithoutDuplicateBranchOrPR(t *testing.T) {
	sha, tree, head := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	c := domain.ChangeSet{SHA: sha, Tree: tree, Changes: []domain.Change{{Path: "new.bin", Mode: "100755", Content: []byte{0, 1, 2}}, {Path: "old.txt", Mode: "100644", Delete: true}}}
	task := domain.Task{ID: strings.Repeat("1", 32), SHA: sha, Spec: domain.Spec{Repository: "https://github.com/TEAM/REPO.git", Prompt: "fix a bug"}}
	p := domain.Publication{TaskID: task.ID, Base: "main", Branch: "dcar/task-" + task.ID, CreatedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	branchExists, prExists := false, false
	branchCreates, prCreates := 0, 0
	var commitRequest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("missing publisher credential")
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			reply(map[string]string{"status": "ahead"})
		case strings.HasSuffix(r.URL.Path, "/git/commits/"+sha):
			reply(map[string]any{"sha": sha, "tree": oid{strings.Repeat("d", 40)}})
		case strings.HasSuffix(r.URL.Path, "/git/blobs"):
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			data, e := base64.StdEncoding.DecodeString(b["content"])
			if e != nil || string(data) != string(c.Changes[0].Content) {
				t.Error("binary content damaged")
			}
			h := sha1.New()
			fmt.Fprintf(h, "blob %d%c", len(data), 0)
			h.Write(data)
			reply(oid{hex.EncodeToString(h.Sum(nil))})
		case strings.HasSuffix(r.URL.Path, "/git/trees"):
			var v struct {
				Entries []treeEntry `json:"tree"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			if len(v.Entries) != 2 || v.Entries[0].Mode != "100755" || v.Entries[1].SHA != nil {
				t.Error("mode/deletion lost")
			}
			reply(oid{tree})
		case strings.HasSuffix(r.URL.Path, "/git/commits"):
			var b any
			_ = json.NewDecoder(r.Body).Decode(&b)
			data, _ := json.Marshal(b)
			if commitRequest != "" && commitRequest != string(data) {
				t.Error("commit changed on replay")
			}
			commitRequest = string(data)
			reply(oid{head})
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			if !branchExists {
				w.WriteHeader(404)
			} else {
				reply(reference{oid{head}})
			}
		case strings.HasSuffix(r.URL.Path, "/git/refs"):
			branchExists = true
			branchCreates++
			w.WriteHeader(500) // Committed remotely; response lost.
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == "GET":
			if r.URL.Query().Get("state") != "all" {
				t.Error("closed PRs must be included")
			}
			if prExists {
				reply([]pull{{9, "https://github.com/team/repo/pull/9"}})
			} else {
				reply([]pull{})
			}
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == "POST":
			var v map[string]any
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v["draft"] != true || v["base"] != "main" {
				t.Error("invalid PR configuration")
			}
			prExists = true
			prCreates++
			w.WriteHeader(500)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	g := GitHub{Client: server.Client(), Endpoint: server.URL, Token: "fixture-secret"}
	for i := 0; i < 2; i++ {
		_, e := g.Publish(context.Background(), task, p, c)
		if e == nil || !Retryable(e) {
			t.Fatal("lost response not retriable", e)
		}
	}
	got, e := g.Publish(context.Background(), task, p, c)
	if e != nil || got.Status != "published" || got.Number != 9 || got.Commit != head {
		t.Fatal(got, e)
	}
	if branchCreates != 1 || prCreates != 1 {
		t.Fatal("duplicate external mutation", branchCreates, prCreates)
	}
}
func TestPublicationRejectsDivergedBaseAndConflictingBranch(t *testing.T) {
	c := domain.ChangeSet{SHA: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), Changes: []domain.Change{{Path: "x", Mode: "100644"}}}
	task := domain.Task{SHA: c.SHA, Spec: domain.Spec{Repository: "https://github.com/team/repo.git"}}
	for _, mode := range []string{"diverged", "conflict", "tree"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				v := any(oid{strings.Repeat("d", 40)})
				switch {
				case strings.Contains(r.URL.Path, "/compare/"):
					status := "ahead"
					if mode == "diverged" {
						status = mode
					}
					v = map[string]string{"status": status}
				case strings.Contains(r.URL.Path, "/git/commits/"):
					v = commit{Tree: oid{strings.Repeat("e", 40)}}
				case strings.HasSuffix(r.URL.Path, "/git/blobs"):
					h := sha1.Sum([]byte("blob 0\x00"))
					v = oid{hex.EncodeToString(h[:])}
				case strings.HasSuffix(r.URL.Path, "/git/trees"):
					value := c.Tree
					if mode == "tree" {
						value = strings.Repeat("f", 40)
					}
					v = oid{value}
				case strings.Contains(r.URL.Path, "/git/ref/"):
					v = reference{oid{strings.Repeat("0", 40)}}
				}
				_ = json.NewEncoder(w).Encode(v)
			}))
			defer server.Close()
			g := GitHub{Client: server.Client(), Endpoint: server.URL}
			_, e := g.Publish(context.Background(), task, domain.Publication{Base: "main", Branch: "dcar/task-id"}, c)
			if !errors.Is(e, ErrPermanent) || Retryable(e) {
				t.Fatal("unsafe publication accepted", e)
			}
		})
	}
}
func TestGitHubPermissionAndRateErrors(t *testing.T) {
	for _, limited := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limited {
				w.Header().Set("X-RateLimit-Remaining", "0")
			}
			w.WriteHeader(403)
			fmt.Fprint(w, "fixture-secret")
		}))
		g := GitHub{Client: server.Client(), Endpoint: server.URL, Token: "fixture-secret"}
		e := g.request(context.Background(), "GET", "/repos/team/repo", nil, nil)
		server.Close()
		if Retryable(e) != limited || strings.Contains(e.Error(), "fixture-secret") {
			t.Fatal(e)
		}
	}
}
