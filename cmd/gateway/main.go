package main

import (
	"github.com/dcar/runtime/internal/client"
	"github.com/dcar/runtime/internal/config"
	"github.com/dcar/runtime/internal/gateway"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	g := &gateway.Gateway{Control: client.New(config.Env("CONTROL_URL", "http://api:8080"), config.Secret("WORKER_TOKEN")), APIKey: config.Secret("MODEL_API_KEY"), Upstream: config.Env("MODEL_UPSTREAM", "https://api.openai.com/v1"), Hosts: strings.Split(config.Env("EGRESS_HOSTS", gateway.DefaultHosts), ",")}
	s := &http.Server{Addr: ":8081", Handler: g, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	if e := s.ListenAndServe(); e != nil {
		slog.Error("gateway", "error", e)
		os.Exit(1)
	}
}
