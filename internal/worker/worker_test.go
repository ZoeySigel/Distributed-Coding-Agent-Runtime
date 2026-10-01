package worker

import (
	"github.com/dcar/runtime/internal/runner"
	"strings"
	"testing"
)

func TestObserverDoesNotPublishEncodedReports(t *testing.T) {
	var lines []string
	o := &lineObserver{emit: func(s string) { lines = append(lines, s) }}
	for _, s := range []string{"{\"ty", "pe\":\"turn.completed\"}\n", runner.Marker[:4], runner.Marker[4:] + strings.Repeat("secret", 20000) + "\n", "next\n"} {
		o.write([]byte(s))
	}
	if len(lines) != 2 || lines[1] != "next" {
		t.Fatalf("leaked marker/report: %d", len(lines))
	}
}
func TestRetryClassification(t *testing.T) {
	if transient(nilError("authentication failed")) {
		t.Fatal("auth retried")
	}
	if !transient(nilError("connection reset by peer")) {
		t.Fatal("network failure not retried")
	}
	if !transient(nilError("TLS connect error: unexpected eof while reading")) {
		t.Fatal("TLS network interruption not retried")
	}
	if transient(nilError("SSL certificate problem: certificate has expired")) {
		t.Fatal("certificate configuration error retried")
	}
	if got := redact("a token b", "token"); got != "a [REDACTED] b" {
		t.Fatal(got)
	}
}

type nilError string

func (e nilError) Error() string { return string(e) }
