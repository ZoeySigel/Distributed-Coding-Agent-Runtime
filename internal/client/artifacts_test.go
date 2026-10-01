package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dcar/runtime/internal/domain"
)

func TestDownloadRejectsUnsafeManifestCorruptionAndOverwrite(t *testing.T) {
	contents := []byte("patch")
	hash := sha256.Sum256(contents)
	for _, tc := range []struct {
		name, sha string
		size      int64
		bad       bool
	}{
		{"changes.patch", hex.EncodeToString(hash[:]), 5, false},
		{"../escape", hex.EncodeToString(hash[:]), 5, true},
		{"..", hex.EncodeToString(hash[:]), 5, true},
		{"changes.patch", "bad", 5, true},
		{"changes.patch", hex.EncodeToString(hash[:]), 33 << 20, true},
	} {
		t.Run(tc.name+tc.sha, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token" {
					t.Error("missing authentication")
				}
				if r.URL.Path == "/v1/tasks/id/artifacts" {
					_ = json.NewEncoder(w).Encode([]domain.Artifact{{Name: tc.name, SHA256: tc.sha, Size: tc.size}})
					return
				}
				_, _ = w.Write(contents)
			}))
			defer s.Close()
			c := New(s.URL, "token")
			dir := t.TempDir()
			count, err := c.Download(context.Background(), "id", dir)
			if tc.bad {
				if err == nil {
					t.Fatal("accepted invalid manifest")
				}
				entries, _ := os.ReadDir(dir)
				if len(entries) != 0 {
					t.Fatal("wrote unverified file")
				}
				return
			}
			if err != nil || count != 1 {
				t.Fatalf("%d %v", count, err)
			}
			if _, err = c.Download(context.Background(), "id", dir); err == nil {
				t.Fatal("overwrote file")
			}
			b, _ := os.ReadFile(filepath.Join(dir, tc.name))
			if string(b) != "patch" {
				t.Fatal("changed existing file")
			}
		})
	}
}
