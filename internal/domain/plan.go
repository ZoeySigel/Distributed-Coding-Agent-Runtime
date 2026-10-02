package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// ExecutionPlan belongs to the immutable baseline, not to a model turn or worker.
type ExecutionPlan struct {
	SHA            string `json:"sha"`
	Source         string `json:"source"`
	PrepareCommand string `json:"prepare_command"`
	TestCommand    string `json:"test_command"`
	Kind           string `json:"kind"`
	Rationale      string `json:"rationale"`
}

func (p ExecutionPlan) Validate() error {
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(p.SHA) {
		return fmt.Errorf("plan requires a fixed baseline SHA")
	}
	if p.Source != "detected" && p.Source != "agent" && p.Source != "explicit" {
		return fmt.Errorf("invalid plan source")
	}
	switch p.Kind {
	case "tests", "build", "lint", "static", "custom", "unavailable":
	default:
		return fmt.Errorf("invalid verification kind")
	}
	for _, command := range []string{p.PrepareCommand, p.TestCommand} {
		if len(command) > 8192 || strings.ContainsRune(command, 0) {
			return fmt.Errorf("invalid plan command")
		}
	}
	if len(p.Rationale) > 4096 || strings.TrimSpace(p.Rationale) == "" {
		return fmt.Errorf("plan requires a bounded explanation")
	}
	if p.Kind == "unavailable" {
		if p.TestCommand != "" {
			return fmt.Errorf("unavailable plan cannot contain a verification command")
		}
	} else if strings.TrimSpace(p.TestCommand) == "" {
		return fmt.Errorf("verification command required")
	}
	if p.Source == "agent" && p.Kind != "unavailable" {
		// This rejects obvious no-op proposals, not arbitrary shell semantics.
		// Commands still run as untrusted repository code inside the workspace.
		first := strings.Fields(p.TestCommand)[0]
		switch first {
		case "true", "false", ":", "echo", "printf", "exit", "ls", "pwd", "cat":
			return fmt.Errorf("planner proposed a non-verifying command")
		}
	}
	return nil
}
