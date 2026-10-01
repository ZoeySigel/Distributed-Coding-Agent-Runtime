package main

import (
	"context"
	"fmt"
	"github.com/dcar/runtime/internal/artifact"
	"github.com/dcar/runtime/internal/config"
	"github.com/dcar/runtime/internal/control"
	"github.com/dcar/runtime/internal/store"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil {
		slog.Error("server stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, e := store.Open(ctx, config.Secret("DATABASE_URL"))
	if e != nil {
		return e
	}
	defer db.Pool.Close()
	if e = db.Migrate(ctx); e != nil {
		return e
	}
	objects, e := artifact.New(ctx, config.Env("S3_ENDPOINT", "minio:9000"), config.Secret("S3_ACCESS_KEY"), config.Secret("S3_SECRET_KEY"), config.Env("S3_BUCKET", "dcar"), config.Env("S3_TLS", "false") == "true")
	if e != nil {
		return e
	}
	profiles, e := config.Profiles()
	if e != nil {
		return e
	}
	tokens := map[string]string{}
	if e = config.JSONFile(config.Env("API_TOKENS_FILE", "secrets/api-tokens.json"), &tokens); e != nil {
		return e
	}
	creds := map[string]string{}
	if p := os.Getenv("REPO_CREDENTIALS_FILE"); p != "" {
		if e = config.JSONFile(p, &creds); e != nil {
			return e
		}
	}
	workerToken := config.Secret("WORKER_TOKEN")
	if len(workerToken) < 32 {
		return fmt.Errorf("WORKER_TOKEN must contain at least 32 characters")
	}
	seen := map[string]bool{}
	if len(tokens) == 0 {
		return fmt.Errorf("at least one API caller is required")
	}
	for name, v := range tokens {
		if len(v) < 32 {
			return fmt.Errorf("API tokens must contain at least 32 characters")
		}
		if name == "" || seen[v] || v == workerToken {
			return fmt.Errorf("API caller names and tokens must be nonempty and unique; worker token must be separate")
		}
		seen[v] = true
	}
	s := &control.Server{DB: db, Objects: objects, Tokens: tokens, WorkerToken: workerToken, Profiles: profiles, Credentials: creds}
	httpServer := &http.Server{Addr: config.Env("LISTEN", ":8080"), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	var jobs sync.WaitGroup
	jobs.Add(2)
	go func() {
		defer jobs.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if n, e := db.Reap(ctx); e != nil {
					slog.Error("reaper", "error", e)
				} else if n > 0 {
					slog.Info("reconciled tasks", "count", n)
				}
			}
		}
	}()
	go func() {
		defer jobs.Done()
		gc := time.NewTicker(time.Hour)
		defer gc.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-gc.C:
				if e := objects.Sweep(ctx, db); e != nil {
					slog.Error("artifact GC", "error", e)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = httpServer.Shutdown(shutdown)
	}()
	slog.Info("control plane listening", "address", httpServer.Addr)
	e = httpServer.ListenAndServe()
	stop()
	jobs.Wait()
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
