// Package shell handles local command execution requested by the server agent.
package shell

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/internal/config"
)

// Result holds the output of an executed command.
type Result struct {
	Command  string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Executor runs shell commands locally.
type Executor struct {
	cfg   config.ShellConfig
	shell string
}

// NewExecutor creates a new shell executor.
func NewExecutor(cfg config.ShellConfig) *Executor {
	shell := cfg.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	return &Executor{
		cfg:   cfg,
		shell: shell,
	}
}

// Execute runs a command string in the configured shell.
func (e *Executor) Execute(ctx context.Context, command string, workDir string) (*Result, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, e.shell, "-c", command)
	if workDir != "" {
		cmd.Dir = workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &Result{
		Command:  command,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return result, fmt.Errorf("execute command: %w", err)
		}
	}

	return result, nil
}

// IsAllowed checks if a command is in the allowed list.
func (e *Executor) IsAllowed(command string) bool {
	cmdBase := extractBaseCommand(command)
	for _, allowed := range e.cfg.AllowedCommands {
		if cmdBase == allowed {
			return true
		}
	}
	return false
}

// IsBlocked checks if a command matches any blocked pattern.
func (e *Executor) IsBlocked(command string) bool {
	for _, blocked := range e.cfg.BlockedCommands {
		if strings.Contains(command, blocked) {
			return true
		}
	}
	return false
}

// extractBaseCommand gets the first word of a command string.
func extractBaseCommand(command string) string {
	parts := strings.Fields(strings.TrimSpace(command))
	if len(parts) == 0 {
		return ""
	}
	// Handle sudo prefix
	if parts[0] == "sudo" && len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}
