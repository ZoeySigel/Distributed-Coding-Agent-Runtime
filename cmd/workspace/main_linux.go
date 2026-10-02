//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/runner"
	"net/http"
	"os"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	if os.Args[1] == "supervise" {
		supervise()
		return
	}
	if len(os.Args) != 3 {
		os.Exit(2)
	}
	in, e := runner.ReadInput(os.Args[2])
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	ctx := context.Background()
	var v any
	switch os.Args[1] {
	case "resolve":
		v, e = runner.Resolve(ctx, in)
	case "prepare":
		v, e = runner.Prepare(ctx, in)
	case "shell":
		v, e = runner.Shell(ctx, in)
	case "agent":
		v, e = runner.Agent(ctx, in)
	case "plan":
		v, e = runner.Plan(ctx, in)
	case "collect":
		v, e = runner.Collect(ctx, in)
	default:
		e = fmt.Errorf("unknown operation")
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if e = runner.Emit(v); e != nil {
		os.Exit(1)
	}
}
func supervise() { // PID 1 exits when its lease can no longer be established. Docker then kills every remaining process.
	client := &http.Client{Timeout: 3 * time.Second}
	expiry := time.Now().Add(10 * time.Second)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	poll := 0
	for range tick.C {
		if time.Now().After(expiry) {
			return
		}
		for {
			var st syscall.WaitStatus
			pid, _ := syscall.Wait4(-1, &st, syscall.WNOHANG, nil)
			if pid <= 0 {
				break
			}
		}
		poll++
		if poll%3 != 1 {
			continue
		}
		started := time.Now()
		req, _ := http.NewRequest("GET", "http://gateway:8081/lease", nil)
		req.Header.Set("Authorization", "Bearer "+os.Getenv("DCAR_ATTEMPT_TOKEN"))
		res, e := client.Do(req)
		if e != nil {
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return
		}
		var l struct {
			Remaining float64 `json:"remaining_seconds"`
		}
		e = json.NewDecoder(res.Body).Decode(&l)
		res.Body.Close()
		if e == nil {
			expiry = started.Add(time.Duration(l.Remaining * float64(time.Second))).Add(-2 * time.Second)
		}
	}
}
