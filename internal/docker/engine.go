// Package docker uses the Engine HTTP API. It never shells out to docker.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Engine struct {
	HTTP *http.Client
	Base string
}

func New(host string) (*Engine, error) {
	if strings.HasPrefix(host, "unix://") {
		socket := strings.TrimPrefix(host, "unix://")
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		return &Engine{&http.Client{Transport: tr}, "http://docker/v1.45"}, nil
	}
	return nil, fmt.Errorf("worker requires a local Linux Docker unix socket; remote unauthenticated TCP is unsupported")
}
func (e *Engine) call(ctx context.Context, method, path string, in, out any) error {
	var b io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		b = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, e.Base+path, b)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := e.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified && method == "POST" && (strings.HasSuffix(path, "/stop?t=10") || strings.HasSuffix(path, "/start")) {
		return nil // Engine lifecycle operations are idempotent.
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("docker %s: %d %s", path, res.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}
func (e *Engine) Network(ctx context.Context, name, gateway string, labels map[string]string) (string, error) {
	var v struct {
		ID string `json:"Id"`
	}
	err := e.call(ctx, "POST", "/networks/create", map[string]any{"Name": name, "Internal": true, "EnableIPv6": false, "Options": map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": "isolated"}, "Labels": labels, "Driver": "bridge"}, &v)
	if err != nil {
		return "", err
	}
	if err = e.call(ctx, "POST", "/networks/"+v.ID+"/connect", map[string]any{"Container": gateway, "EndpointConfig": map[string]any{"Aliases": []string{"gateway"}}}, nil); err != nil {
		_ = e.call(ctx, "DELETE", "/networks/"+v.ID, nil, nil)
		return "", err
	}
	return v.ID, nil
}
func (e *Engine) RemoveNetwork(ctx context.Context, id, gateway string) error {
	_ = e.call(ctx, "POST", "/networks/"+id+"/disconnect", map[string]any{"Container": gateway, "Force": true}, nil)
	return e.call(ctx, "DELETE", "/networks/"+id, nil, nil)
}
func (e *Engine) Volume(ctx context.Context, name string, labels map[string]string) error {
	return e.call(ctx, "POST", "/volumes/create", map[string]any{"Name": name, "Labels": labels}, nil)
}
func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	return e.call(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil, nil)
}

type Mount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}
type Container struct {
	Image, Name, Network string
	Profile              domain.Profile
	Env, Command         []string
	Mounts               []Mount
	Labels               map[string]string
}

func (e *Engine) Create(ctx context.Context, c Container) (string, error) {
	mounts := []map[string]any{}
	for _, m := range c.Mounts {
		mounts = append(mounts, map[string]any{"Type": m.Type, "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly})
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := e.call(ctx, "POST", "/containers/create?name="+url.QueryEscape(c.Name), map[string]any{"Image": c.Image, "User": "1000:1000", "Cmd": c.Command, "Env": c.Env, "Labels": c.Labels, "WorkingDir": "/workspace", "HostConfig": map[string]any{"NetworkMode": c.Network, "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "NanoCpus": c.Profile.CPU * 1e9, "Memory": c.Profile.Memory, "MemorySwap": c.Profile.Memory, "PidsLimit": c.Profile.PIDs, "Mounts": mounts, "Tmpfs": map[string]string{"/tmp": "rw,exec,nosuid,nodev,size=536870912,uid=1000,gid=1000", "/home/agent": "rw,exec,nosuid,nodev,size=268435456,uid=1000,gid=1000"}, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "2"}}}}, &out)
	return out.ID, err
}
func (e *Engine) Start(ctx context.Context, id string) error {
	return e.call(ctx, "POST", "/containers/"+id+"/start", nil, nil)
}
func (e *Engine) Stop(ctx context.Context, id string) error {
	return e.call(ctx, "POST", "/containers/"+id+"/stop?t=10", nil, nil)
}
func (e *Engine) Remove(ctx context.Context, id string) error {
	return e.call(ctx, "DELETE", "/containers/"+id+"?force=true", nil, nil)
}
func (e *Engine) Copy(ctx context.Context, id, path, name string, data []byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Uid: 1000, Gid: 1000, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, "PUT", e.Base+"/containers/"+id+"/archive?path="+url.QueryEscape(path), &buf)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/x-tar")
	res, err := e.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("docker copy: %s %s", res.Status, b)
	}
	return nil
}

type Result struct {
	Code      int
	Output    []byte
	Truncated bool
}

func (e *Engine) Exec(ctx context.Context, id string, argv []string, env []string, limit int) (Result, error) {
	return e.ExecObserved(ctx, id, argv, env, limit, nil)
}
func (e *Engine) ExecObserved(ctx context.Context, id string, argv []string, env []string, limit int, observe func([]byte)) (Result, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err := e.call(ctx, "POST", "/containers/"+id+"/exec", map[string]any{"AttachStdout": true, "AttachStderr": true, "Cmd": argv, "Env": env, "User": "1000:1000"}, &created); err != nil {
		return Result{}, err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", e.Base+"/exec/"+created.ID+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return Result{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := e.HTTP.Do(r)
	if err != nil {
		return Result{}, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return Result{}, fmt.Errorf("docker exec start: %s", res.Status)
	}
	var reader io.Reader = res.Body
	if observe != nil {
		reader = &observedFrames{source: res.Body, observe: observe}
	}
	out, truncated, err := Demux(reader, limit)
	if err != nil {
		return Result{}, err
	}
	var state struct {
		ExitCode int
		Running  bool
	}
	if err = e.call(ctx, "GET", "/exec/"+created.ID+"/json", nil, &state); err != nil {
		return Result{}, err
	}
	if state.Running {
		return Result{}, fmt.Errorf("exec stream closed before process exit")
	}
	var container struct{ State struct{ Running bool } }
	if err = e.call(ctx, "GET", "/containers/"+id+"/json", nil, &container); err != nil {
		return Result{}, err
	}
	if !container.State.Running {
		return Result{}, fmt.Errorf("workspace process interrupted: container exited")
	}
	return Result{state.ExitCode, out, truncated}, nil
}

// Forward only frame payload bytes to the event observer; preserve headers for Demux.
type observedFrames struct {
	source    io.Reader
	observe   func([]byte)
	remaining int64
	header    [8]byte
	headerPos int
}

func (o *observedFrames) Read(p []byte) (int, error) {
	if o.remaining == 0 {
		if o.headerPos == 0 {
			if _, e := io.ReadFull(o.source, o.header[:]); e != nil {
				return 0, e
			}
			o.remaining = int64(binary.BigEndian.Uint32(o.header[4:]))
			if o.remaining > 64<<20 {
				return 0, fmt.Errorf("invalid docker frame")
			}
		}
		n := copy(p, o.header[o.headerPos:])
		o.headerPos += n
		if o.headerPos == 8 {
			o.headerPos = 0
		}
		return n, nil
	}
	if o.headerPos != 0 {
		n := copy(p, o.header[o.headerPos:])
		o.headerPos += n
		if o.headerPos == 8 {
			o.headerPos = 0
		}
		return n, nil
	}
	if int64(len(p)) > o.remaining {
		p = p[:o.remaining]
	}
	n, e := o.source.Read(p)
	o.remaining -= int64(n)
	if n > 0 {
		o.observe(p[:n])
	}
	return n, e
}
func Demux(r io.Reader, limit int) ([]byte, bool, error) {
	var b bytes.Buffer
	truncated := false
	for {
		header := make([]byte, 8)
		_, e := io.ReadFull(r, header)
		if e == io.EOF {
			break
		}
		if e != nil {
			return b.Bytes(), truncated, e
		}
		n := int64(binary.BigEndian.Uint32(header[4:]))
		if n > 64<<20 {
			return nil, false, fmt.Errorf("invalid docker frame")
		}
		keep := min(n, int64(max(0, limit-b.Len())))
		if _, e = io.CopyN(&b, r, keep); e != nil {
			return b.Bytes(), truncated, e
		}
		if n > keep {
			truncated = true
			if _, e = io.CopyN(io.Discard, r, n-keep); e != nil {
				return b.Bytes(), truncated, e
			}
		}
	}
	return b.Bytes(), truncated, nil
}

type Listed struct {
	ID     string `json:"Id"`
	Names  []string
	Labels map[string]string
}

func (e *Engine) Containers(ctx context.Context, worker string) ([]Listed, error) {
	f, _ := json.Marshal(map[string][]string{"label": {"dcar.worker=" + worker}})
	var v []Listed
	err := e.call(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(f)), nil, &v)
	return v, err
}
func (e *Engine) Networks(ctx context.Context, worker string) ([]Listed, error) {
	f, _ := json.Marshal(map[string][]string{"label": {"dcar.worker=" + worker}})
	var v []Listed
	err := e.call(ctx, "GET", "/networks?filters="+url.QueryEscape(string(f)), nil, &v)
	return v, err
}
func (e *Engine) Volumes(ctx context.Context, worker string) ([]struct {
	Name   string
	Labels map[string]string
}, error) {
	f, _ := json.Marshal(map[string][]string{"label": {"dcar.worker=" + worker}})
	var v struct {
		Volumes []struct {
			Name   string
			Labels map[string]string
		}
	}
	err := e.call(ctx, "GET", "/volumes?filters="+url.QueryEscape(string(f)), nil, &v)
	return v.Volumes, err
}
func (e *Engine) Ping(ctx context.Context) error {
	ctx, c := context.WithTimeout(ctx, 5*time.Second)
	defer c()
	var version struct {
		Version string
		Os      string
	}
	if err := e.call(ctx, "GET", "/version", nil, &version); err != nil {
		return err
	}
	var major int
	if _, err := fmt.Sscanf(version.Version, "%d.", &major); err != nil || major < 28 || version.Os != "linux" {
		return fmt.Errorf("requires Linux Docker Engine 28+ for isolated workspace networks; got %s %s", version.Os, version.Version)
	}
	return nil
}
