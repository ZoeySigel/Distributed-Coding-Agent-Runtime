package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Change struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Content []byte `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}
type ChangeSet struct {
	SHA     string   `json:"sha"`
	Tree    string   `json:"tree"`
	Changes []Change `json:"changes"`
}

func (c ChangeSet) Validate() error {
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(c.SHA) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(c.Tree) || len(c.Changes) > 1000 {
		return fmt.Errorf("invalid publication baseline, tree or file count")
	}
	seen := map[string]bool{}
	size := 0
	for _, f := range c.Changes {
		if f.Path == "" || !utf8.ValidString(f.Path) || strings.HasPrefix(f.Path, "/") || strings.ContainsAny(f.Path, "\\:\x00\r\n") || seen[f.Path] {
			return fmt.Errorf("invalid publication path")
		}
		for _, p := range strings.Split(f.Path, "/") {
			if p == "" || p == "." || p == ".." || strings.EqualFold(p, ".git") {
				return fmt.Errorf("unsafe publication path")
			}
		}
		seen[f.Path] = true
		if f.Mode != "100644" && f.Mode != "100755" && f.Mode != "120000" {
			return fmt.Errorf("unsupported publication mode")
		}
		if f.Delete && len(f.Content) != 0 {
			return fmt.Errorf("deleted entry contains data")
		}
		size += len(f.Content)
		if size > 16<<20 {
			return fmt.Errorf("publication contents exceed 16 MiB")
		}
	}
	return nil
}

type Publication struct {
	TaskID    string    `json:"task_id"`
	Status    string    `json:"status"`
	Branch    string    `json:"branch"`
	Base      string    `json:"base,omitempty"`
	URL       string    `json:"url,omitempty"`
	Number    int       `json:"number,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Error     string    `json:"error,omitempty"`
	Attempts  int       `json:"attempts"`
	Token     string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}
