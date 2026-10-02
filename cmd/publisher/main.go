package main

import (
	"context"
	"github.com/dcar/runtime/internal/artifact"
	"github.com/dcar/runtime/internal/config"
	"github.com/dcar/runtime/internal/publisher"
	"github.com/dcar/runtime/internal/store"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil {
		slog.Error("publisher stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	ctx, c := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer c()
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
	repos, e := publisher.LoadRepositories(config.Env("PUBLICATION_REPOSITORIES_FILE", "/run/publication/repositories.json"))
	if e != nil {
		return e
	}
	s := publisher.Service{DB: db, Objects: objects, Repositories: repos}
	return s.Run(ctx)
}
