package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dcar/runtime/internal/domain"
)

// Download verifies the manifest before writing and never overwrites existing files.
func (c *Client) Download(ctx context.Context, id, directory string) (int, error) {
	var artifacts []domain.Artifact
	if err := c.Do(ctx, "GET", "/v1/tasks/"+id+"/artifacts", nil, &artifacts, nil); err != nil {
		return 0, err
	}
	if len(artifacts) == 0 {
		return 0, fmt.Errorf("no artifacts available yet")
	}
	for _, a := range artifacts {
		if a.Name == "." || a.Name == ".." || filepath.Base(a.Name) != a.Name || strings.ContainsAny(a.Name, "/\\:") || a.Name == "" {
			return 0, fmt.Errorf("unsafe artifact name")
		}
		if a.Size < 0 || a.Size > 32<<20 {
			return 0, fmt.Errorf("artifact exceeds download limit: %s", a.Name)
		}
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return 0, err
	}
	for i, a := range artifacts {
		req, err := http.NewRequestWithContext(ctx, "GET", c.URL+"/v1/tasks/"+id+"/artifacts/"+a.Name, nil)
		if err != nil {
			return i, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		res, err := c.HTTP.Do(req)
		if err != nil {
			return i, err
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return i, &Error{res.StatusCode, res.Status}
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
		res.Body.Close()
		if err != nil {
			return i, err
		}
		hash := sha256.Sum256(b)
		if int64(len(b)) != a.Size || hex.EncodeToString(hash[:]) != a.SHA256 {
			return i, fmt.Errorf("artifact checksum mismatch: %s", a.Name)
		}
		path := filepath.Join(directory, a.Name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return i, err
		}
		_, err = f.Write(b)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
			return i, err
		}
	}
	return len(artifacts), nil
}
