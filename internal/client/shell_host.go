package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// ShellBackend executes a shell command on behalf of the sidecar.
// Registered via ProcessClient.SetShellBackend. When unset, the
// sidecar's `shell.run` host RPC returns `no_shell_backend` and the
// sidecar falls back to its own in-process exec path (LocalIde).
//
// Implementations should honour the provided ctx deadline and return
// an error only for process-spawn failures; a non-zero exit should be
// captured in the result's ExitCode, not the error.
type ShellBackend interface {
	Run(ctx context.Context, params protocol.ShellRunParams) (protocol.ShellRunResult, error)
}

// SetShellBackend registers the host-side shell executor. TUI callers
// wire this so commands the agent issues via `run_command` actually
// run in the user's shell, with their cwd/env/credentials. Headless
// callers (banya run, eval harness, vibesynth codegen) leave this
// unset; banya-core falls back to its internal LocalIde exec.
func (c *ProcessClient) SetShellBackend(s ShellBackend) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shell = s
}

// handleShellRun satisfies the sidecar's `shell.run` host RPC.
func (c *ProcessClient) handleShellRun(req protocol.SidecarLine) {
	if c.shell == nil {
		c.writeResponse(req.ID, nil, &protocol.ErrorData{
			Code:    "no_shell_backend",
			Message: "no ShellBackend registered on host",
		})
		return
	}
	var params protocol.ShellRunParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			c.writeResponse(req.ID, nil, &protocol.ErrorData{
				Code:    "bad_params",
				Message: err.Error(),
			})
			return
		}
	}
	if params.Command == "" {
		c.writeResponse(req.ID, nil, &protocol.ErrorData{
			Code:    "bad_params",
			Message: "shell.run requires a non-empty command",
		})
		return
	}

	timeout := time.Duration(params.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	c.hostCalls.Store(req.ID, cancel)
	defer func() {
		cancel()
		c.hostCalls.Delete(req.ID)
	}()

	result, err := c.shell.Run(ctx, params)
	if err != nil {
		c.writeResponse(req.ID, nil, &protocol.ErrorData{
			Code:    "shell_backend_error",
			Message: err.Error(),
		})
		return
	}
	c.writeResponse(req.ID, result, nil)
}

// LocalShellBackend runs commands via the user's $SHELL (falling back
// to /bin/sh), inheriting banya-cli's env + cwd. That means commands
// see the same PATH, HOME, OAuth tokens, SSH keys, and working
// directory the user launched banya from — matching user expectation
// for "run this in my terminal".
//
// The subprocess has its stdin wired to /dev/null (claude CLI-style —
// interactive TUI commands like `vim` won't work in this path; that's
// a conscious MVP tradeoff and flagged to the user in the description).
// stdout/stderr are captured in full and returned as strings.
type LocalShellBackend struct {
	shell string
}

// NewLocalShellBackend constructs a LocalShellBackend. Empty shell
// resolves to $SHELL (env), then /bin/sh as a final fallback.
func NewLocalShellBackend(shell string) *LocalShellBackend {
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	return &LocalShellBackend{shell: shell}
}

// Run implements ShellBackend.
func (b *LocalShellBackend) Run(ctx context.Context, params protocol.ShellRunParams) (protocol.ShellRunResult, error) {
	// Route GUI / long-running / interactive commands to a fresh
	// Terminal.app window so pygame/tkinter/dev-server etc. actually
	// display and the user can Ctrl-C them independently. The agent
	// receives an immediate "spawned" response so it doesn't wait on
	// stdout that would never arrive.
	if shouldSpawnInTerminal(params.Command) {
		return b.spawnInNewTerminal(ctx, params)
	}
	return b.runInProcess(ctx, params)
}

// runInProcess is the historical path: spawn via $SHELL -c, capture
// stdout/stderr, return the result synchronously. Suitable for
// git/grep/pip/ls and any command where the agent needs the output.
func (b *LocalShellBackend) runInProcess(ctx context.Context, params protocol.ShellRunParams) (protocol.ShellRunResult, error) {
	start := time.Now()

	cmd := exec.CommandContext(ctx, b.shell, "-c", params.Command)
	if params.Cwd != "" {
		cmd.Dir = params.Cwd
	}
	cmd.Env = os.Environ()
	// Disconnect stdin — subprocess has no TTY to interact with. Commands
	// that block on stdin (reading a password, running an editor) will
	// exit immediately rather than hanging the agent turn.
	if dev, err := os.Open(os.DevNull); err == nil {
		cmd.Stdin = dev
		defer dev.Close()
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	elapsed := time.Since(start)

	res := protocol.ShellRunResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ElapsedMs: elapsed.Milliseconds(),
	}

	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		// Process-spawn failure: shell binary missing, perms, etc.
		return res, fmt.Errorf("shell exec: %w", runErr)
	}
	return res, nil
}

// spawnInNewTerminal opens a fresh Terminal.app window on macOS and
// runs the command there. Returns immediately once the window is
// scheduled; the agent never sees the subprocess's stdout because the
// command now belongs to the user's interactive terminal, not the
// agent's captured pipe. Linux/Windows fall back to runInProcess —
// those platforms can add their own spawner (x-terminal-emulator,
// wt/cmd start) when we need them.
func (b *LocalShellBackend) spawnInNewTerminal(ctx context.Context, params protocol.ShellRunParams) (protocol.ShellRunResult, error) {
	if runtime.GOOS != "darwin" {
		// Outside macOS we don't have a universal "open a terminal
		// window" primitive yet — fall back to in-process so the
		// command at least executes, even if the GUI-window case
		// doesn't work as the user expected.
		return b.runInProcess(ctx, params)
	}
	start := time.Now()
	// Build the shell snippet that the new Terminal window runs:
	//   cd <cwd> && <user command>
	// so the launched shell inherits the caller's working directory
	// without needing an absolute path in every user command.
	cwd := params.Cwd
	if cwd == "" {
		if d, err := os.Getwd(); err == nil {
			cwd = d
		}
	}
	inner := params.Command
	if cwd != "" {
		inner = "cd " + osaQuote(cwd) + " && " + inner
	}
	script := fmt.Sprintf(
		`tell application "Terminal" to do script %s
tell application "Terminal" to activate`,
		osaQuote(inner),
	)
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		// Osascript failed — fall back so the user still sees *some*
		// output rather than a silent no-op.
		return b.runInProcess(ctx, params)
	}
	return protocol.ShellRunResult{
		ExitCode:  0,
		Stdout:    "[spawned in new Terminal.app window: " + params.Command + "]",
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

// shouldSpawnInTerminal decides whether a command is "GUI-ish" enough
// to deserve its own Terminal.app window. Kept deliberately narrow so
// routine tool calls (git status, ls, grep, pip install, pytest) stay
// in-process and the agent keeps getting their output. Opt-in for
// anyone: prefix the command with `!term ` to force the new-terminal
// path regardless of shape.
func shouldSpawnInTerminal(command string) bool {
	trimmed := strings.TrimSpace(command)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "!term ") || strings.HasPrefix(lower, "!terminal ") {
		return true
	}
	// macOS `open` launches an app/URL — always a foreground UX.
	if strings.HasPrefix(lower, "open ") {
		return true
	}
	// Dev servers that want a live console: `npm run dev`, `npm start`,
	// `yarn dev`, `pnpm dev`, `bun dev`, `cargo run`, `./gradlew bootRun`,
	// `flutter run`.
	for _, prefix := range []string{
		"npm run dev", "npm start", "yarn dev", "yarn start",
		"pnpm dev", "pnpm start", "bun dev", "bun run dev",
		"cargo run", "flutter run", "./gradlew bootrun",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	// Python scripts invoked as `python <file>.py` or `python3 …` are
	// often pygame / tkinter GUIs. `python -c "..."`, `python -m pip`,
	// etc. stay in-process because they're short-lived / CLI.
	if (strings.HasPrefix(lower, "python ") || strings.HasPrefix(lower, "python3 ")) &&
		strings.Contains(lower, ".py") &&
		!strings.Contains(lower, " -c ") &&
		!strings.Contains(lower, " -m pip") {
		return true
	}
	return false
}

// osaQuote escapes a Go string for safe embedding inside an osascript
// string literal. AppleScript uses double quotes and backslash-escapes
// double quotes and backslashes.
func osaQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
