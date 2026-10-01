package runner

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Resolve is separate from checkout so the control plane pins the SHA before preparation.
func Resolve(ctx context.Context, in Input) (Prepared, error) {
	if in.SHA != "" {
		return Prepared{SHA: in.SHA}, nil
	}
	if regexp.MustCompile(`^[a-fA-F0-9]{40}$`).MatchString(in.Ref) {
		return Prepared{SHA: strings.ToLower(in.Ref)}, nil
	}
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if in.Credential != "" {
		if e := os.WriteFile("/tmp/askpass", []byte("#!/bin/sh\ncase \"$1\" in *Username*) printf '%s' 'x-access-token';; *) printf '%s' \"$DCAR_GIT_TOKEN\";; esac\n"), 0700); e != nil {
			return Prepared{}, e
		}
		defer os.Remove("/tmp/askpass")
		env = append(env, "GIT_ASKPASS=/tmp/askpass", "DCAR_GIT_TOKEN="+in.Credential)
	}
	ref := in.Ref
	if ref == "" {
		ref = "HEAD"
	}
	patterns := []string{ref}
	if ref != "HEAD" && !strings.HasPrefix(ref, "refs/") {
		patterns = []string{"refs/heads/" + ref, "refs/tags/" + ref, "refs/tags/" + ref + "^{}"}
	} else if strings.HasPrefix(ref, "refs/tags/") {
		patterns = append(patterns, ref+"^{}")
	}
	b, e := run(ctx, "/tmp", env, append([]string{"git", "ls-remote", "--exit-code", "--", in.Repository}, patterns...)...)
	if e != nil {
		msg := e.Error()
		if in.Credential != "" {
			msg = strings.ReplaceAll(msg, in.Credential, "[REDACTED]")
		}
		return Prepared{}, fmt.Errorf("resolve: %s", msg)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			refs[fields[1]] = fields[0]
		}
	}
	if strings.HasPrefix(ref, "refs/") || ref == "HEAD" {
		sha := refs[ref]
		if peeled := refs[ref+"^{}"]; peeled != "" {
			sha = peeled
		}
		if sha == "" {
			return Prepared{}, fmt.Errorf("ref not found")
		}
		return Prepared{SHA: sha}, nil
	}
	branch, tag := refs["refs/heads/"+ref], refs["refs/tags/"+ref]
	if branch != "" && tag != "" {
		return Prepared{}, fmt.Errorf("ambiguous ref: specify refs/heads/ or refs/tags/")
	}
	if peeled := refs["refs/tags/"+ref+"^{}"]; peeled != "" {
		tag = peeled
	}
	if branch != "" {
		return Prepared{SHA: branch}, nil
	}
	if tag != "" {
		return Prepared{SHA: tag}, nil
	}
	return Prepared{}, fmt.Errorf("ref not found")
}
