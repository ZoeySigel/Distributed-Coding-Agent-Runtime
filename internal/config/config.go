package config

import (
	"encoding/json"
	"fmt"
	"github.com/dcar/runtime/internal/domain"
	"os"
	"strings"
)

func Env(k, fallback string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return fallback
}
func Secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		b, e := os.ReadFile(p)
		if e != nil {
			panic(e)
		}
		return strings.TrimSpace(string(b))
	}
	return os.Getenv(k)
}
func JSONFile(path string, out any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, out)
}
func Profiles() (map[string]domain.Profile, error) {
	p := map[string]domain.Profile{}
	if e := JSONFile(Env("PROFILES_FILE", "config/profiles.json"), &p); e != nil {
		return nil, e
	}
	for name, v := range p {
		if name == "" || v.Image == "" || v.Model == "" || (v.Executor != "codex" && v.Executor != "fixture") {
			return nil, fmt.Errorf("invalid profile %s", name)
		}
		if v.CPU == 0 {
			v.CPU = 2
		}
		if v.Memory == 0 {
			v.Memory = 4 << 30
		}
		if v.PIDs == 0 {
			v.PIDs = 256
		}
		if v.CPU < 1 || v.Memory < 128<<20 || v.PIDs < 16 {
			return nil, fmt.Errorf("invalid resource limits")
		}
		p[name] = v
	}
	return p, nil
}
