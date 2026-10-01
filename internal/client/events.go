package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Events reopens interrupted streams using the last delivered cursor. Callbacks run in event order.
func (c *Client) Events(ctx context.Context, id string, after int64, follow bool, event func(domain.Event), terminal func(domain.Task)) error {
	stream := *c.HTTP
	stream.Timeout = 0
	failures := 0
	for {
		req, e := http.NewRequestWithContext(ctx, "GET", c.URL+"/v1/tasks/"+id+"/events", nil)
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
		res, e := stream.Do(req)
		if e == nil && res.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			res.Body.Close()
			e = &Error{Status: res.StatusCode, Body: string(b)}
			if res.StatusCode < 500 {
				return e
			}
		}
		if e == nil {
			scanner := bufio.NewScanner(res.Body)
			scanner.Buffer(make([]byte, 4096), 2<<20)
			kind := ""
			data := ""
			done := false
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					if data != "" {
						switch kind {
						case "execution":
							var v domain.Event
							if e = json.Unmarshal([]byte(data), &v); e != nil {
								break
							}
							if v.ID > after {
								event(v)
								after = v.ID
							}
							failures = 0
						case "terminal":
							var t domain.Task
							if e = json.Unmarshal([]byte(data), &t); e == nil {
								terminal(t)
								done = true
							}
						}
					}
					kind = ""
					data = ""
					if e != nil || done {
						break
					}
					continue
				}
				if strings.HasPrefix(line, ": heartbeat") && !follow {
					done = true
					break
				}
				if v, ok := strings.CutPrefix(line, "event: "); ok {
					kind = v
				}
				if v, ok := strings.CutPrefix(line, "data: "); ok {
					if data != "" {
						data += "\n"
					}
					data += v
				}
			}
			if e == nil {
				e = scanner.Err()
			}
			res.Body.Close()
			if done {
				return e
			}
			if e == nil {
				e = io.ErrUnexpectedEOF
			}
		}
		if !follow {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failures++
		if failures > 5 {
			return fmt.Errorf("event stream reconnect exhausted at cursor %d: %w", after, e)
		}
		t := time.NewTimer(time.Duration(failures) * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
