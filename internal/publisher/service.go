package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/artifact"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/store"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type Repository struct {
	TokenFile string `json:"token_file"`
	Base      string `json:"base,omitempty"`
}
type Service struct {
	DB           *store.Store
	Objects      artifact.ArtifactStore
	Repositories map[string]Repository
}

func (s *Service) Run(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			p, e := s.DB.ClaimPublication(ctx)
			if e != nil {
				slog.Error("publication claim failed", "error", e)
				continue
			}
			if p == nil {
				continue
			}
			s.execute(ctx, *p)
		}
	}
}
func (s *Service) execute(parent context.Context, p domain.Publication) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	lease := p
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if e := s.DB.RenewPublication(ctx, lease); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	status := "failed"
	retry := false
	t, e := s.DB.PublicationTask(ctx, p)
	if e == nil && t.Status != "succeeded" {
		e = reject("only successful tasks can be published")
	}
	var changes domain.ChangeSet
	if e == nil {
		var artifacts []domain.Artifact
		artifacts, e = s.DB.Artifacts(ctx, t.ID)
		if e == nil {
			found := false
			for _, a := range artifacts {
				if a.Name != "publication.json" {
					continue
				}
				found = true
				var r io.ReadCloser
				r, e = s.Objects.Get(ctx, a.Key)
				if e != nil {
					break
				}
				var b []byte
				b, e = io.ReadAll(io.LimitReader(r, artifact.MaxSize+1))
				r.Close()
				if e != nil {
					break
				}
				if int64(len(b)) != a.Size || store.Hash(b) != a.SHA256 {
					e = reject("publication artifact checksum mismatch")
					break
				}
				e = json.Unmarshal(b, &changes)
				if e == nil {
					e = changes.Validate()
				}
				if e != nil {
					e = reject("invalid publication artifact")
				}
				break
			}
			if !found {
				e = reject("publication artifact missing")
			}
		}
	}
	if e == nil && changes.SHA != t.SHA {
		e = reject("publication artifact baseline mismatch")
	}
	if e == nil && len(changes.Changes) == 0 {
		status = "no_changes"
	} else if e == nil {
		key := strings.ToLower(RepoKey(t.Spec.Repository))
		cfg, ok := s.Repositories[key]
		if !ok {
			e = reject("repository not configured for GitHub write access")
		} else {
			var token []byte
			token, e = os.ReadFile(cfg.TokenFile)
			if e != nil {
				e = reject("GitHub token file unavailable")
			} else if strings.TrimSpace(string(token)) == "" {
				e = reject("GitHub token file is empty")
			} else {
				g := GitHub{Client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Endpoint: "https://api.github.com", Token: strings.TrimSpace(string(token))}
				if p.Base == "" {
					p.Base = cfg.Base
					if p.Base == "" {
						p.Base, e = g.DefaultBase(ctx, RepoKey(t.Spec.Repository))
					}
					if e == nil {
						e = s.DB.FixPublicationBase(ctx, p, p.Base)
					}
				}
				if e == nil {
					p, e = g.Publish(ctx, t, p, changes)
					if e == nil {
						status = p.Status
					}
				}
			}
		}
	}
	if e != nil {
		p.Error = e.Error()
		if len(p.Error) > 1024 {
			p.Error = p.Error[:1024]
		}
		retry = Retryable(e)
		slog.Warn("publication failed", "task", p.TaskID, "retry", retry, "error", p.Error)
	}
	if ctx.Err() != nil {
		return
	} // Let lease recovery handle interrupted or stale publishers.
	if e := s.DB.FinishPublication(ctx, p, status, retry); e != nil {
		slog.Warn("publication receipt rejected", "task", p.TaskID, "error", e)
	} else {
		slog.Info("publication finished", "task", p.TaskID, "status", status, "url", p.URL)
	}
}
func LoadRepositories(path string) (map[string]Repository, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var raw map[string]Repository
	if e = json.Unmarshal(b, &raw); e != nil {
		return nil, e
	}
	out := map[string]Repository{}
	for k, v := range raw {
		spec := domain.Spec{Repository: "https://github.com/" + k, Prompt: "validate"}
		if e := spec.Normalize(); e != nil || v.TokenFile == "" {
			return nil, fmt.Errorf("invalid publisher repository configuration")
		}
		k = strings.ToLower(RepoKey(spec.Repository))
		if _, ok := out[k]; ok {
			return nil, fmt.Errorf("duplicate publisher repository")
		}
		if v.Base != "" {
			spec.PRBase = v.Base
			if e := spec.Normalize(); e != nil {
				return nil, e
			}
		}
		out[k] = v
	}
	return out, nil
}
