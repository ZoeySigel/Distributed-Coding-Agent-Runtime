package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func frame(s string) []byte {
	b := make([]byte, 8)
	b[0] = 1
	binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
	return append(b, []byte(s)...)
}

func TestStopAlreadyStopped(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotModified) }))
	defer s.Close()
	e := &Engine{HTTP: s.Client(), Base: s.URL}
	if err := e.Stop(context.Background(), "container"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(context.Background(), "container"); err == nil {
		t.Fatal("unexpected status must remain an error")
	}
}
func TestDemuxDrainsAndLimits(t *testing.T) {
	raw := append(frame("hello"), frame("world")...)
	r := bytes.NewReader(raw)
	b, tr, e := Demux(r, 7)
	if e != nil || !tr || string(b) != "hellowo" || r.Len() != 0 {
		t.Fatalf("%q %t %v remaining %d", b, tr, e, r.Len())
	}
}
func TestDemuxTruncatedFrame(t *testing.T) {
	_, _, e := Demux(bytes.NewReader(frame("hello")[:10]), 100)
	if e == nil {
		t.Fatal("accepted broken frame")
	}
}
func TestObservedFrameBoundaries(t *testing.T) {
	raw := append(append(frame("hello"), frame("")...), frame("world")...)
	var observed bytes.Buffer
	r := &observedFrames{source: bytes.NewReader(raw), observe: func(b []byte) { observed.Write(b) }}
	var reconstructed bytes.Buffer
	buf := make([]byte, 3)
	for {
		n, e := r.Read(buf)
		reconstructed.Write(buf[:n])
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	if !bytes.Equal(raw, reconstructed.Bytes()) || observed.String() != "helloworld" {
		t.Fatalf("corrupted frames: %q", observed.String())
	}
}
