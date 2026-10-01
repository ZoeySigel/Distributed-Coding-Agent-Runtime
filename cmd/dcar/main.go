package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/config"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if e := run(); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, e)
		code := 1
		var httpErr *client.Error
		var cliErr *exitError
		if errors.As(e, &cliErr) {
			code = cliErr.code
		} else if errors.As(e, &httpErr) {
			switch httpErr.Status {
			case 400, 409:
				code = 2
			case 401, 403:
				code = 3
			case 404:
				code = 5
			}
		}
		os.Exit(code)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return &exitError{2, "usage: dcar submit|list|status|logs|cancel|retry|download [flags]"}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := client.New(config.Env("DCAR_URL", "http://localhost:8080"), config.Secret("DCAR_TOKEN"))
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	id := fs.String("id", "", "task id")
	key := fs.String("key", "", "idempotency key (generated if omitted)")
	jsonOutput := fs.Bool("json", false, "JSON output")
	file := fs.String("file", "", "submission JSON file")
	repo := fs.String("repo", "", "GitHub HTTPS URL")
	prompt := fs.String("prompt", "", "natural language task")
	ref := fs.String("ref", "", "branch, tag or commit")
	profile := fs.String("profile", "default", "execution profile")
	test := fs.String("test", "", "test command override")
	prepare := fs.String("prepare", "", "dependency preparation command")
	credential := fs.String("credential", "", "server-side credential reference")
	timeout := fs.Int("timeout", 3600, "total deadline in seconds")
	testTimeout := fs.Int("test-timeout", 0, "test timeout in seconds (default min(600,total timeout))")
	follow := fs.Bool("follow", false, "stream until terminal")
	after := fs.String("after", "0", "last persisted event id")
	outdir := fs.String("out", "results", "download directory")
	offset := fs.Int("offset", 0, "list offset")
	limit := fs.Int("limit", 50, "list page size (1..100)")
	if e := fs.Parse(os.Args[2:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return e
		}
		return &exitError{2, e.Error()}
	}
	if fs.NArg() != 0 {
		return &exitError{2, "unexpected positional arguments"}
	}
	var out any
	if *id != "" {
		if len(*id) != 32 || strings.ContainsAny(*id, "/\\?#") {
			return &exitError{2, "invalid task id"}
		}
	}
	switch cmd {
	case "submit":
		s := domain.Spec{Repository: *repo, Prompt: *prompt, Ref: *ref, Profile: *profile, TestCommand: *test, PrepareCommand: *prepare, CredentialRef: *credential, TimeoutSeconds: *timeout, TestTimeoutSeconds: *testTimeout}
		if *file != "" {
			if e := config.JSONFile(*file, &s); e != nil {
				return e
			}
		}
		if *key == "" {
			*key = domain.ID()
			fmt.Fprintln(os.Stderr, "Idempotency-Key:", *key)
		}
		if e := c.Do(ctx, "POST", "/v1/tasks", s, &out, map[string]string{"Idempotency-Key": *key}); e != nil {
			return e
		}
	case "list":
		if *limit < 1 || *limit > 100 || *offset < 0 {
			return &exitError{2, "invalid list pagination"}
		}
		if e := c.Do(ctx, "GET", fmt.Sprintf("/v1/tasks?offset=%d&limit=%d", *offset, *limit), nil, &out, nil); e != nil {
			return e
		}
	case "status", "cancel", "retry":
		if *id == "" {
			return &exitError{2, "--id required"}
		}
		path := "/v1/tasks/" + *id
		method := "GET"
		headers := map[string]string{}
		if cmd != "status" {
			method = "POST"
			path += "/" + cmd
		}
		if cmd == "retry" {
			if *key == "" {
				*key = domain.ID()
				fmt.Fprintln(os.Stderr, "Idempotency-Key:", *key)
			}
			headers["Idempotency-Key"] = *key
		}
		if e := c.Do(ctx, method, path, nil, &out, headers); e != nil {
			return e
		}
	case "logs":
		if *id == "" {
			return &exitError{2, "--id required"}
		}
		cursor, e := strconv.ParseInt(*after, 10, 64)
		if e != nil || cursor < 0 {
			return &exitError{2, "invalid --after cursor"}
		}
		var result domain.Task
		e = c.Events(ctx, *id, cursor, *follow, func(v domain.Event) {
			if *jsonOutput {
				_ = json.NewEncoder(os.Stdout).Encode(v)
			} else {
				fmt.Printf("%d [%s] %s\n", v.ID, v.Kind, v.Data)
			}
		}, func(t domain.Task) {
			result = t
			if *jsonOutput {
				_ = json.NewEncoder(os.Stdout).Encode(t)
			} else {
				fmt.Printf("Task %s: %s\n", t.ID, t.Status)
			}
		})
		if e != nil {
			return e
		}
		if result.Status != "" && result.Status != "succeeded" {
			return &exitError{4, "task ended: " + result.Status}
		}
		return nil
	case "download":
		if *id == "" {
			return &exitError{2, "--id required"}
		}
		var artifacts []domain.Artifact
		if e := c.Do(ctx, "GET", "/v1/tasks/"+*id+"/artifacts", nil, &artifacts, nil); e != nil {
			return e
		}
		if e := os.MkdirAll(*outdir, 0755); e != nil {
			return e
		}
		for _, a := range artifacts {
			if filepath.Base(a.Name) != a.Name || strings.ContainsAny(a.Name, "/\\:") {
				return fmt.Errorf("unsafe artifact name")
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", c.URL+"/v1/tasks/"+*id+"/artifacts/"+a.Name, nil)
			req.Header.Set("Authorization", "Bearer "+c.Token)
			res, e := c.HTTP.Do(req)
			if e != nil {
				return e
			}
			if res.StatusCode != 200 {
				res.Body.Close()
				return &client.Error{Status: res.StatusCode, Body: res.Status}
			}
			b, e := io.ReadAll(io.LimitReader(res.Body, 33<<20))
			res.Body.Close()
			if e != nil {
				return e
			}
			h := sha256.Sum256(b)
			if hex.EncodeToString(h[:]) != a.SHA256 || int64(len(b)) != a.Size {
				return fmt.Errorf("artifact checksum mismatch: %s", a.Name)
			}
			path := filepath.Join(*outdir, a.Name)
			f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, e = f.Write(b)
			ce := f.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		}
		out = map[string]any{"directory": *outdir, "artifacts": len(artifacts)}
	default:
		return &exitError{2, "unknown command " + cmd}
	}
	enc := json.NewEncoder(os.Stdout)
	if !*jsonOutput {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(out)
}

type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }
