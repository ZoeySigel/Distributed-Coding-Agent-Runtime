package client

import (
	"context"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestEventsReconnectAtDeliveredCursor(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "id: 41\nevent: execution\ndata: {\"id\":41,\"kind\":\"log\",\"data\":\"one\"}\n\n")
			return
		}
		if r.Header.Get("Last-Event-ID") != "41" {
			t.Errorf("wrong cursor %s", r.Header.Get("Last-Event-ID"))
		}
		fmt.Fprint(w, "id: 42\nevent: execution\ndata: {\"id\":42,\"kind\":\"log\",\"data\":\"two\"}\n\nevent: terminal\ndata: {\"id\":\"task\",\"status\":\"succeeded\"}\n\n")
	}))
	defer s.Close()
	c := New(s.URL, "token")
	var ids []int64
	terminal := ""
	e := c.Events(context.Background(), "task", 0, true, func(v domain.Event) { ids = append(ids, v.ID) }, func(v domain.Task) { terminal = v.Status })
	if e != nil || len(ids) != 2 || ids[0] != 41 || ids[1] != 42 || terminal != "succeeded" {
		t.Fatal(ids, terminal, e)
	}
}
