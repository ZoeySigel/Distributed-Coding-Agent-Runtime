package gateway

import (
	"context"
	"encoding/json"
	"github.com/dcar/runtime/internal/client"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicIPAndAllowlist(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "::1", "fc00::1", "fe80::1", "0.0.0.0", "224.0.0.1"} {
		if PublicIP(net.ParseIP(s)) {
			t.Errorf("accepted %s", s)
		}
	}
	if !PublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public IP rejected")
	}
	g := &Gateway{Hosts: []string{"github.com", "*.githubusercontent.com"}}
	for _, s := range []string{"evilgithub.com", "github.com.evil", "localhost"} {
		if g.Allowed(s) {
			t.Errorf("accepted %s", s)
		}
	}
	if !g.Allowed("raw.githubusercontent.com") {
		t.Fatal("wildcard rejected")
	}
	if _, e := g.dial(context.Background(), "tcp", "github.com:22"); e == nil {
		t.Fatal("SSH allowed")
	}
}
func TestRevokedLeaseAndModelRestrictions(t *testing.T) {
	valid := true
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !valid {
			w.WriteHeader(409)
			return
		}
		_ = json.NewEncoder(w).Encode(Lease{AttemptID: "a", Model: "allowed", Remaining: 30})
	}))
	defer control.Close()
	g := &Gateway{Control: client.New(control.URL, "worker"), APIKey: "secret", Upstream: "https://api.openai.com/v1"}
	for _, path := range []string{"/v1/files", "/v1/responses"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"forbidden"}`))
		r.Header.Set("Authorization", "Bearer attempt")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 403 && w.Code != 404 {
			t.Fatalf("unexpected %d", w.Code)
		}
	}
	valid = false
	r := httptest.NewRequest("GET", "/lease", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("revoked lease accepted")
	}
}

func TestProxyChallengesBeforeLeaseLookup(t *testing.T) {
	g := &Gateway{}
	r := httptest.NewRequest("CONNECT", "https://github.com:443", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusProxyAuthRequired || w.Header().Get("Proxy-Authenticate") == "" {
		t.Fatalf("missing standard proxy challenge: %d", w.Code)
	}
}
