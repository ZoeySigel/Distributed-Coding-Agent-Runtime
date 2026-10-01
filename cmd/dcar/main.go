package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/config"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/tui"
	"os"
	"os/signal"
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	settings, e := config.LoadClientSettings()
	if e != nil {
		return e
	}
	c := client.New(config.Env("DCAR_URL", settings.URL), config.Secret("DCAR_TOKEN"))
	cmd := "ui"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
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
	tokenFile := fs.String("token-file", settings.TokenFile, "API token JSON path (local entry; environment token takes precedence)")
	apiURL := fs.String("url", c.URL, "API endpoint")
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return e
		}
		return &exitError{2, e.Error()}
	}
	if fs.NArg() != 0 {
		return &exitError{2, "unexpected positional arguments"}
	}
	var out any
	c.URL = strings.TrimRight(*apiURL, "/")
	if cmd == "configure" {
		path, e := config.SaveClientSettings(config.ClientSettings{URL: *apiURL, TokenFile: *tokenFile})
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"config": path})
	}
	if c.Token == "" {
		token, e := config.ReadClientToken(*tokenFile)
		if e == nil {
			c.Token = token
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	if *id != "" {
		if len(*id) != 32 || strings.ContainsAny(*id, "/\\?#") {
			return &exitError{2, "invalid task id"}
		}
	}
	switch cmd {
	case "ui", "tui":
		if *jsonOutput {
			return &exitError{2, "UI does not support --json; use list/status/logs for scripts"}
		}
		s := domain.Spec{Repository: *repo, Prompt: *prompt, Ref: *ref, Profile: *profile, TestCommand: *test, PrepareCommand: *prepare, CredentialRef: *credential, TimeoutSeconds: *timeout, TestTimeoutSeconds: *testTimeout}
		if *file != "" {
			if e := config.JSONFile(*file, &s); e != nil {
				return e
			}
		}
		note := ""
		if s.Repository == "" {
			repository, sha, detected := config.GitHubWorkingRepository(ctx)
			s.Repository = repository
			if s.Ref == "" {
				s.Ref = sha
			}
			note = detected
		}
		return tui.Run(ctx, c, tui.Options{Spec: s, TokenFile: *tokenFile, Output: *outdir, Note: note})
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
		count, e := c.Download(ctx, *id, *outdir)
		if e != nil {
			return e
		}
		out = map[string]any{"directory": *outdir, "artifacts": count}
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
