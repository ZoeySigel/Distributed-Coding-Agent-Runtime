package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	URL, Token string
	HTTP       *http.Client
}
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }
func New(url, token string) *Client {
	return &Client{url, token, &http.Client{Timeout: 20 * time.Second}}
}
func (c *Client) Do(ctx context.Context, method, path string, in, out any, headers map[string]string) error {
	var b io.Reader
	if in != nil {
		raw, e := json.Marshal(in)
		if e != nil {
			return e
		}
		b = bytes.NewReader(raw)
	}
	r, e := http.NewRequestWithContext(ctx, method, c.URL+path, b)
	if e != nil {
		return e
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, e := c.HTTP.Do(r)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return &Error{res.StatusCode, string(b)}
	}
	if out != nil && res.StatusCode != 204 {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}
