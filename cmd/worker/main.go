package main

import (
	"context"
	"fmt"
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/config"
	"github.com/dcar/runtime/internal/docker"
	"github.com/dcar/runtime/internal/domain"
	"github.com/dcar/runtime/internal/worker"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil {
		slog.Error("worker stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	ctx, c := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer c()
	id := os.Getenv("WORKER_ID")
	if id == "" {
		return fmt.Errorf("WORKER_ID must be stable and unique per Docker Engine")
	}
	cap, e := strconv.Atoi(config.Env("WORKER_CAPACITY", "2"))
	if e != nil {
		return e
	}
	engine, e := docker.New(config.Env("DOCKER_HOST", "unix:///var/run/docker.sock"))
	if e != nil {
		return e
	}
	w := &worker.Worker{Control: client.New(config.Env("CONTROL_URL", "http://api:8080"), config.Secret("WORKER_TOKEN")), Engine: engine, Gateway: config.Env("GATEWAY_CONTAINER", "dcar-gateway"), Registration: domain.Registration{ID: id, Session: domain.ID(), Capacity: cap, Profiles: strings.Split(config.Env("WORKER_PROFILES", "default"), ",")}}
	s := &http.Server{Addr: ":9091", Handler: http.HandlerFunc(w.Metrics), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.ListenAndServe() }()
	defer s.Close()
	return w.Run(ctx)
}
