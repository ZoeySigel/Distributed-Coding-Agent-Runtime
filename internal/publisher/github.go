// Package publisher publishes only immutable, authoritative successful results.
// It never executes repository code and never forwards GitHub write credentials
// to workers or workspaces.
package publisher

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type GitHub struct {
	Client   *http.Client
	Endpoint string
	Token    string
}
type HTTPError struct {
	Status int
	Retry  bool
}

func (e *HTTPError) Error() string { return fmt.Sprintf("GitHub HTTP %d", e.Status) }
func Retryable(e error) bool {
	var h *HTTPError
	if errors.As(e, &h) {
		return h.Retry
	}
	return !errors.Is(e, ErrPermanent)
}

var ErrPermanent = errors.New("publication rejected")

func reject(s string) error { return fmt.Errorf("%w: %s", ErrPermanent, s) }
func (g *GitHub) request(ctx context.Context, method, path string, in, out any) error {
	var data []byte
	var e error
	if in != nil {
		data, e = json.Marshal(in)
		if e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, g.Endpoint+path, bytes.NewReader(data))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, e := g.Client.Do(req)
	if e != nil {
		return fmt.Errorf("GitHub transport failed: %w", e)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 65536))
		return &HTTPError{res.StatusCode, res.StatusCode == 429 || res.StatusCode >= 500 || (res.StatusCode == 403 && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != ""))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
}
func RepoKey(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(repository, "https://github.com/"), ".git")
}

type oid struct {
	SHA string `json:"sha"`
}
type commit struct {
	SHA  string `json:"sha"`
	Tree oid    `json:"tree"`
}
type reference struct {
	Object oid `json:"object"`
}
type treeEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
}
type pull struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
}

func (g *GitHub) DefaultBase(ctx context.Context, key string) (string, error) {
	var repo struct {
		Default string `json:"default_branch"`
	}
	e := g.request(ctx, "GET", "/repos/"+key, nil, &repo)
	if e == nil && repo.Default == "" {
		e = reject("repository has no default branch")
	}
	return repo.Default, e
}

// Content-addressed Git objects and a deterministic task branch make external
// mutation replayable even if GitHub committed a request but its response was lost.
func (g *GitHub) Publish(ctx context.Context, t domain.Task, p domain.Publication, c domain.ChangeSet) (domain.Publication, error) {
	if e := c.Validate(); e != nil {
		return p, reject(e.Error())
	}
	if c.SHA != t.SHA {
		return p, reject("artifact baseline mismatch")
	}
	if len(c.Changes) == 0 {
		p.Status = "no_changes"
		return p, nil
	}
	key := RepoKey(t.Spec.Repository)
	root := "/repos/" + key
	var comparison struct {
		Status string `json:"status"`
	}
	if e := g.request(ctx, "GET", root+"/compare/"+c.SHA+"..."+url.PathEscape(p.Base), nil, &comparison); e != nil {
		return p, e
	}
	if comparison.Status != "ahead" && comparison.Status != "identical" {
		return p, reject("baseline is not an ancestor of PR base; specify pr_base")
	}
	var base commit
	if e := g.request(ctx, "GET", root+"/git/commits/"+c.SHA, nil, &base); e != nil {
		return p, e
	}
	entries := make([]treeEntry, 0, len(c.Changes))
	for _, f := range c.Changes {
		entry := treeEntry{Path: f.Path, Mode: f.Mode, Type: "blob"}
		if !f.Delete {
			var blob oid
			if e := g.request(ctx, "POST", root+"/git/blobs", map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(f.Content)}, &blob); e != nil {
				return p, e
			}
			h := sha1.New()
			fmt.Fprintf(h, "blob %d%c", len(f.Content), 0)
			h.Write(f.Content)
			if blob.SHA != hex.EncodeToString(h.Sum(nil)) {
				return p, reject("GitHub blob integrity mismatch")
			}
			entry.SHA = &blob.SHA
		}
		entries = append(entries, entry)
	}
	var tree oid
	if e := g.request(ctx, "POST", root+"/git/trees", map[string]any{"base_tree": base.Tree.SHA, "tree": entries}, &tree); e != nil {
		return p, e
	}
	if tree.SHA != c.Tree {
		return p, reject("GitHub tree differs from trusted collected tree")
	}
	date := p.CreatedAt.UTC().Format(time.RFC3339)
	identity := map[string]string{"name": "DCAR", "email": "dcar@users.noreply.github.com", "date": date}
	var head oid
	if e := g.request(ctx, "POST", root+"/git/commits", map[string]any{"message": "DCAR task " + t.ID, "tree": tree.SHA, "parents": []string{c.SHA}, "author": identity, "committer": identity}, &head); e != nil {
		return p, e
	}
	p.Commit = head.SHA
	path := root + "/git/ref/heads/" + p.Branch
	var ref reference
	e := g.request(ctx, "GET", path, nil, &ref)
	var httpErr *HTTPError
	if errors.As(e, &httpErr) && httpErr.Status == 404 {
		e = g.request(ctx, "POST", root+"/git/refs", map[string]string{"ref": "refs/heads/" + p.Branch, "sha": head.SHA}, nil)
		// A race or lost create response is resolved by reading the deterministic ref.
		if e != nil && !(errors.As(e, &httpErr) && httpErr.Status == 422) {
			return p, e
		}
		e = g.request(ctx, "GET", path, nil, &ref)
	}
	if e != nil {
		return p, e
	}
	if ref.Object.SHA != head.SHA {
		return p, reject("task branch has conflicting changes; refusing to overwrite")
	}
	// Search all states, including closed/merged PRs. A replay never reopens or
	// recreates a PR that a human has already handled.
	pullsPath := root + "/pulls?state=all&head=" + url.QueryEscape(strings.Split(key, "/")[0]+":"+p.Branch) + "&per_page=100"
	var prs []pull
	if e := g.request(ctx, "GET", pullsPath, nil, &prs); e != nil {
		return p, e
	}
	var pr pull
	if len(prs) > 0 {
		pr = prs[0]
	} else {
		title := t.Spec.PRTitle
		if title == "" {
			title = "DCAR: " + strings.Split(t.Spec.Prompt, "\n")[0]
			r := []rune(title)
			if len(r) > 120 {
				title = string(r[:120])
			}
		}
		coverage := "unknown"
		if t.Plan != nil {
			coverage = t.Plan.Kind
		}
		prompt := t.Spec.Prompt
		if len(prompt) > 32000 {
			end := 32000
			for !utf8.RuneStart(prompt[end]) {
				end--
			}
			prompt = prompt[:end] + "\n[Task description truncated; full text is in DCAR.]"
		}
		body := fmt.Sprintf("Automated coding task `%s`.\n\n%s\n\nBaseline: `%s`\nVerification coverage: **%s**. Build/lint/static checks do not prove functional correctness.\n\nPatch, logs and independent baseline/final verification reports are available in DCAR task artifacts.\n", t.ID, prompt, t.SHA, coverage)
		e := g.request(ctx, "POST", root+"/pulls", map[string]any{"title": title, "body": body, "head": p.Branch, "base": p.Base, "draft": true}, &pr)
		if errors.As(e, &httpErr) && httpErr.Status == 422 {
			var existing []pull
			lookup := g.request(ctx, "GET", pullsPath, nil, &existing)
			if lookup != nil {
				return p, lookup
			}
			if len(existing) > 0 {
				pr = existing[0]
				e = nil
			}
		}
		if e != nil {
			return p, e
		}
	}
	prURL, urlErr := url.Parse(pr.URL)
	if pr.Number < 1 || urlErr != nil || prURL.Scheme != "https" || prURL.Host != "github.com" || !strings.EqualFold(prURL.Path, "/"+key+"/pull/"+strconv.Itoa(pr.Number)) {
		return p, reject("invalid pull request response")
	}
	p.Number = pr.Number
	p.URL = pr.URL
	p.Status = "published"
	p.Error = ""
	return p, nil
}
