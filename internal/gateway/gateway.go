// Package gateway mediates all workspace egress and keeps the upstream model key outside it.
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/client"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Lease struct {
	AttemptID string  `json:"attempt_id"`
	Model     string  `json:"model"`
	Remaining float64 `json:"remaining_seconds"`
}

const DefaultHosts = "github.com,*.githubusercontent.com,registry.npmjs.org,registry.npmmirror.com,cdn.npmmirror.com,pypi.org,files.pythonhosted.org,proxy.golang.org,sum.golang.org,*.golang.org,storage.googleapis.com"

type Gateway struct {
	Control          *client.Client
	APIKey, Upstream string
	Hosts            []string
	mu               sync.Mutex
	active           map[string]int
}

// Dependency downloads must not consume the model's concurrency budget.
func (g *Gateway) acquire(attempt string, proxy bool) (func(), bool) {
	kind, limit := "model", 4
	if proxy {
		kind, limit = "egress", 32
	}
	key := attempt + ":" + kind
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == nil {
		g.active = map[string]int{}
	}
	if g.active[key] >= limit {
		slog.Warn("attempt concurrency limit reached", "attempt", attempt, "kind", kind, "limit", limit)
		return nil, false
	}
	g.active[key]++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.active[key]--
		if g.active[key] == 0 {
			delete(g.active, key)
		}
	}, true
}

func PublicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.Equal(net.ParseIP("169.254.169.254")) && !inCIDR(ip, "100.64.0.0/10") && !inCIDR(ip, "198.18.0.0/15")
}
func inCIDR(ip net.IP, s string) bool { _, n, _ := net.ParseCIDR(s); return n.Contains(ip) }
func (g *Gateway) Allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, h := range g.Hosts {
		if host == h || (strings.HasPrefix(h, "*.") && strings.HasSuffix(host, h[1:])) {
			return true
		}
	}
	return false
}
func (g *Gateway) dial(ctx context.Context, network, address string) (net.Conn, error) {
	h, p, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	if !g.Allowed(h) || (p != "443" && p != "80") {
		return nil, fmt.Errorf("egress destination denied")
	}
	ips, e := net.DefaultResolver.LookupIP(ctx, "ip", h)
	if e != nil {
		return nil, e
	}
	for _, ip := range ips {
		if !PublicIP(ip) {
			return nil, fmt.Errorf("non-public destination denied")
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no destination address")
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), p))
}
func (g *Gateway) lease(ctx context.Context, token string) (Lease, error) {
	var l Lease
	e := g.Control.Do(ctx, "POST", "/internal/lease", map[string]string{"token": token}, &l, nil)
	return l, e
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	proxy := r.Method == "CONNECT" || r.URL.IsAbs()
	if proxy {
		h := strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
		b, _ := base64.StdEncoding.DecodeString(h)
		_, token, _ = strings.Cut(string(b), ":")
		if token == "" {
			w.Header().Set("Proxy-Authenticate", `Basic realm="dcar-attempt"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
	}
	l, e := g.lease(r.Context(), token)
	if e != nil {
		w.WriteHeader(403)
		return
	}
	if !proxy && r.URL.Path == "/lease" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l)
		return
	}
	release, ok := g.acquire(l.AttemptID, proxy)
	if !ok {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		return
	}
	defer release()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, e := g.lease(ctx, token); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	if r.Method == "CONNECT" {
		g.connect(ctx, w, r)
		return
	}
	if proxy {
		if r.URL.Scheme != "http" {
			w.WriteHeader(403)
			return
		}
		req := r.Clone(ctx)
		req.RequestURI = ""
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Authorization")
		tr := &http.Transport{DialContext: g.dial, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		g.forward(w, req, tr)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/responses" {
		w.WriteHeader(404)
		return
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if e != nil {
		w.WriteHeader(413)
		return
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(b, &payload) != nil {
		w.WriteHeader(400)
		return
	}
	var model string
	_ = json.Unmarshal(payload["model"], &model)
	if model != l.Model || model == "" {
		w.WriteHeader(403)
		return
	}
	up, e := url.Parse(g.Upstream)
	if e != nil || up.Scheme != "https" {
		w.WriteHeader(503)
		return
	}
	up.Path = strings.TrimSuffix(up.Path, "/") + "/responses"
	req, e := http.NewRequestWithContext(ctx, "POST", up.String(), strings.NewReader(string(b)))
	if e != nil {
		w.WriteHeader(500)
		return
	}
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", r.Header.Get("Accept"))
	g.forward(w, req, http.DefaultTransport)
}
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, tr http.RoundTripper) {
	res, e := tr.RoundTrip(r)
	if e != nil {
		http.Error(w, "upstream unavailable", 502)
		return
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		slog.Warn("upstream rate limited", "host", r.URL.Hostname())
	}
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	buf := make([]byte, 16384)
	for {
		n, e := res.Body.Read(buf)
		if n > 0 {
			if _, we := w.Write(buf[:n]); we != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if e != nil {
			return
		}
	}
}
func (g *Gateway) connect(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	remote, e := g.dial(ctx, "tcp", r.Host)
	if e != nil {
		w.WriteHeader(403)
		return
	}
	defer remote.Close()
	h, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(500)
		return
	}
	conn, buf, e := h.Hijack()
	if e != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = buf.Flush()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, remote); done <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-done:
	}
}
