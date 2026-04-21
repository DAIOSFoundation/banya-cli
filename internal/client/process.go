package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// ProcessClient communicates with a banya-core sidecar binary via stdio NDJSON
// JSON-RPC. One sidecar process is kept alive for the lifetime of this client.
// The transport is bidirectional: the sidecar may issue RpcRequests back to
// the host (e.g. `llm.chat`) which are dispatched to the registered LLMBackend.
type ProcessClient struct {
	binPath string
	backend LLMBackend
	// extraEnv is merged onto os.Environ() when the sidecar is spawned.
	// Used to pass subagent / critic config (BANYA_SUBAGENT_*) through
	// the /settings TUI without round-tripping through the IPC protocol.
	extraEnv []string
	// stderrSink, when non-nil, receives the sidecar's stderr stream
	// instead of os.Stderr. TUI callers redirect to a log file to keep
	// sidecar chatter (console.log, Bun SEA boot probes, etc.) from
	// tearing the Bubble Tea screen. Headless callers (banya run, eval
	// harnesses) leave this nil so stderr flows through the terminal
	// next to the NDJSON stdout — matching prior behaviour.
	stderrSink io.Writer
	// shell, when non-nil, handles the sidecar's `shell.run` host RPC —
	// i.e. the agent's run_command tool is delegated to the host so the
	// command runs in the user's shell/cwd/env. Only set by TUI callers
	// that want that pass-through semantics; headless callers leave it
	// nil and banya-core falls back to its internal LocalIde exec.
	shell ShellBackend

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Scanner

	pending    sync.Map // id → chan protocol.RpcResponse
	hostCalls  sync.Map // id → context.CancelFunc (in-flight host RPCs)
	events     chan protocol.ServerEvent
	eventsOnce sync.Once
	reqCounter atomic.Uint64
	closed     atomic.Bool

	cancel context.CancelFunc
}

// SetLLMBackend registers a backend to fulfill sidecar-initiated `llm.chat`
// calls. Must be set before the sidecar starts issuing inbound requests.
func (c *ProcessClient) SetLLMBackend(b LLMBackend) { c.backend = b }

// SetExtraEnv stores KEY=VALUE pairs that are appended to the sidecar
// process environment on spawn. Call before the first request so the
// env is present when start() runs. Safe to call with nil/empty slice
// to clear.
func (c *ProcessClient) SetExtraEnv(env []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(env) == 0 {
		c.extraEnv = nil
		return
	}
	c.extraEnv = append([]string(nil), env...)
}

// SetStderrSink redirects the sidecar's stderr to `w`. Must be called
// before the sidecar starts (the first Chat / request). Passing nil
// restores the default (inherit os.Stderr). TUI callers point this at
// a log file to keep sidecar chatter from tearing the Bubble Tea
// screen; headless callers leave it unset.
func (c *ProcessClient) SetStderrSink(w io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stderrSink = w
}

// SubagentEnvVars renders provider/model/apiKey/endpoint as KEY=VALUE
// env strings for the banya-core sidecar. Returns nil if every field
// is empty so callers can skip SetExtraEnv entirely.
//
// Defined here (not in internal/app or cmd/banya) so run.go, serve.go,
// and the TUI path all share one helper without introducing a new
// package or import cycles.
func SubagentEnvVars(provider, model, apiKey, endpoint string) []string {
	if provider == "" && model == "" && apiKey == "" {
		return nil
	}
	env := []string{
		"BANYA_SUBAGENT_PROVIDER=" + provider,
		"BANYA_SUBAGENT_MODEL=" + model,
		"BANYA_SUBAGENT_API_KEY=" + apiKey,
	}
	if endpoint != "" {
		env = append(env, "BANYA_SUBAGENT_ENDPOINT="+endpoint)
	}
	return env
}

// LanguageEnvVar renders the default language preference as a single
// BANYA_LANGUAGE=KEY env string. Returns "" if lang is empty so callers
// can skip the append cleanly.
func LanguageEnvVar(lang string) string {
	if lang == "" {
		return ""
	}
	return "BANYA_LANGUAGE=" + lang
}

// NewProcessClient creates a sidecar-backed Client. binPath may be empty;
// ResolveSidecarPath is used to find the executable.
func NewProcessClient(binPath string) (*ProcessClient, error) {
	resolved, err := ResolveSidecarPath(binPath)
	if err != nil {
		return nil, err
	}
	return &ProcessClient{binPath: resolved}, nil
}

// ResolveSidecarPath locates the banya-core sidecar binary using, in order:
//  1. explicit argument
//  2. BANYA_CORE_BIN env var
//  3. $XDG_DATA_HOME/banya/bin/banya-core-<os>-<arch>
//  4. banya-core-<os>-<arch> on PATH
//  5. banya-core on PATH
//  6. embedded sidecar → auto-extracted to $XDG_DATA_HOME/banya/bin
func ResolveSidecarPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("sidecar not found at %s", explicit)
	}
	if env := os.Getenv("BANYA_CORE_BIN"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env, nil
		}
		return "", fmt.Errorf("BANYA_CORE_BIN=%s does not exist", env)
	}

	binName := platformBinaryName()

	// XDG data dir
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = filepath.Join(home, ".local", "share")
		}
	}
	if dataDir != "" {
		candidate := filepath.Join(dataDir, "banya", "bin", binName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	// PATH
	if p, err := exec.LookPath(binName); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("banya-core"); err == nil {
		return p, nil
	}

	// Embedded bundle (extract once to XDG data dir).
	if p, err := InstallEmbeddedSidecar(); err == nil {
		return p, nil
	}

	return "", fmt.Errorf("banya-core sidecar not found (set BANYA_CORE_BIN or pass --sidecar, or ship a cli built with an embedded sidecar)")
}

// start lazily spawns the sidecar process and launches the reader goroutine.
func (c *ProcessClient) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, c.binPath)
	if len(c.extraEnv) > 0 {
		cmd.Env = append(os.Environ(), c.extraEnv...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if c.stderrSink != nil {
		cmd.Stderr = c.stderrSink
	} else {
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start sidecar: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4MB max line

	c.cmd = cmd
	c.stdin = stdin
	c.reader = scanner
	c.cancel = cancel
	c.events = make(chan protocol.ServerEvent, 64)

	go c.readLoop(stdout)
	return nil
}

// readLoop demultiplexes sidecar stdout into:
//
//   - inbound RpcRequest  (sidecar → host)         — has `method`
//   - RpcResponse         (sidecar reply to host)  — has `id` without `method`
//   - ServerEvent         (streaming from sidecar) — has `type`
func (c *ProcessClient) readLoop(stdout io.ReadCloser) {
	defer func() {
		_ = stdout.Close()
		c.eventsOnce.Do(func() { close(c.events) })
	}()

	for c.reader.Scan() {
		line := c.reader.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw protocol.SidecarLine
		if err := json.Unmarshal(line, &raw); err != nil {
			fmt.Fprintf(os.Stderr, "[banya-cli] unparseable sidecar line: %s\n", string(line))
			continue
		}

		switch {
		case raw.Method != "":
			go c.handleHostRequest(raw)
		case raw.ID != "":
			if ch, ok := c.pending.LoadAndDelete(raw.ID); ok {
				var result any
				if len(raw.Result) > 0 {
					_ = json.Unmarshal(raw.Result, &result)
				}
				ch.(chan protocol.RpcResponse) <- protocol.RpcResponse{
					ID:     raw.ID,
					Result: result,
					Error:  raw.Error,
				}
			}
		case raw.Type != "":
			_ = c.safeEmitEvent(protocol.ServerEvent{
				Type:      raw.Type,
				SessionID: raw.SessionID,
				Data:      raw.Data,
			})
		}
	}
}

// handleHostRequest fulfills an RpcRequest sent by the sidecar back to
// the host (e.g. `llm.chat`). The result is written back on stdin as a
// standard RpcResponse carrying the same id.
func (c *ProcessClient) handleHostRequest(req protocol.SidecarLine) {
	switch req.Method {
	case protocol.MethodLlmChat:
		c.handleLlmChat(req)
	case protocol.MethodLlmCancel:
		c.handleLlmCancel(req)
	case protocol.MethodShellRun:
		c.handleShellRun(req)
	default:
		c.writeResponse(req.ID, nil, &protocol.ErrorData{
			Code:    "method_not_implemented",
			Message: "host method not implemented: " + req.Method,
		})
	}
}

func (c *ProcessClient) handleLlmChat(req protocol.SidecarLine) {
	if c.backend == nil {
		c.writeResponse(req.ID, nil, &protocol.ErrorData{
			Code:    "no_llm_backend",
			Message: "no LLMBackend registered on host",
		})
		return
	}

	var params protocol.LlmChatParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			c.writeResponse(req.ID, nil, &protocol.ErrorData{
				Code:    "bad_params",
				Message: err.Error(),
			})
			return
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.hostCalls.Store(req.ID, cancel)
	defer func() {
		cancel()
		c.hostCalls.Delete(req.ID)
	}()

	content, finish, toolCalls, err := c.backend.Chat(ctx, params, func(token string) error {
		if err := c.safeEmitEvent(protocol.ServerEvent{
			Type:      protocol.EventContentDelta,
			SessionID: req.ID,
			Data:      protocol.ContentDelta{Content: token},
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		c.writeResponse(req.ID, nil, &protocol.ErrorData{
			Code:    "llm_backend_error",
			Message: err.Error(),
		})
		return
	}
	c.writeResponse(req.ID, protocol.LlmChatResult{
		Content:      content,
		FinishReason: finish,
		ToolCalls:    toolCalls,
	}, nil)
}

func (c *ProcessClient) handleLlmCancel(req protocol.SidecarLine) {
	var p protocol.LlmCancelParams
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	cancelled := false
	if v, ok := c.hostCalls.Load(p.RequestID); ok {
		v.(context.CancelFunc)()
		cancelled = true
	}
	c.writeResponse(req.ID, map[string]bool{"cancelled": cancelled}, nil)
}

// safeEmitEvent sends an event on c.events but recovers if the channel
// is already closed (Close() raced with an in-flight host llm.chat
// callback). Returns io.ErrClosedPipe-style sentinel so the backend
// can abort its stream cleanly.
func (c *ProcessClient) safeEmitEvent(evt protocol.ServerEvent) (err error) {
	if c.closed.Load() {
		return io.ErrClosedPipe
	}
	defer func() {
		if r := recover(); r != nil {
			err = io.ErrClosedPipe
		}
	}()
	c.events <- evt
	return nil
}

// writeResponse writes an RpcResponse to the sidecar's stdin.
func (c *ProcessClient) writeResponse(id string, result any, errData *protocol.ErrorData) {
	resp := protocol.RpcResponse{ID: id, Result: result, Error: errData}
	body, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[banya-cli] marshal host response: %v\n", err)
		return
	}
	body = append(body, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		return
	}
	if _, err := c.stdin.Write(body); err != nil {
		fmt.Fprintf(os.Stderr, "[banya-cli] write host response: %v\n", err)
	}
}

// call sends an RpcRequest and waits for the matching RpcResponse.
func (c *ProcessClient) call(method string, params any, timeout time.Duration) (*protocol.RpcResponse, error) {
	if err := c.start(); err != nil {
		return nil, err
	}

	id := fmt.Sprintf("r%d", c.reqCounter.Add(1))
	replyCh := make(chan protocol.RpcResponse, 1)
	c.pending.Store(id, replyCh)

	req := protocol.RpcRequest{ID: id, Method: method, Params: params}
	body, err := json.Marshal(req)
	if err != nil {
		c.pending.Delete(id)
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	body = append(body, '\n')

	c.mu.Lock()
	_, err = c.stdin.Write(body)
	c.mu.Unlock()
	if err != nil {
		c.pending.Delete(id)
		return nil, fmt.Errorf("write request: %w", err)
	}

	select {
	case resp := <-replyCh:
		if resp.Error != nil {
			return &resp, fmt.Errorf("sidecar error [%s]: %s", resp.Error.Code, resp.Error.Message)
		}
		return &resp, nil
	case <-time.After(timeout):
		c.pending.Delete(id)
		return nil, fmt.Errorf("rpc timeout after %s (method=%s)", timeout, method)
	}
}

// SendMessage starts a chat turn on the sidecar and returns the shared
// event channel. Events for the turn arrive on this channel until `done`.
func (c *ProcessClient) SendMessage(req protocol.ChatRequest) (<-chan protocol.ServerEvent, error) {
	if _, err := c.call(protocol.MethodChatStart, req, 30*time.Second); err != nil {
		return nil, err
	}
	return c.events, nil
}

// ListSessions fetches saved-conversation metadata from the sidecar.
func (c *ProcessClient) ListSessions() ([]protocol.SessionSummary, error) {
	resp, err := c.call(protocol.MethodSessionList, nil, 5*time.Second)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshal session.list result: %w", err)
	}
	var parsed protocol.SessionListResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal session.list: %w", err)
	}
	return parsed.Sessions, nil
}

// LoadSession pulls the conversation history for the given id.
func (c *ProcessClient) LoadSession(id string) ([]protocol.Message, error) {
	params := protocol.SessionLoadParams{SessionID: id}
	resp, err := c.call(protocol.MethodSessionLoad, params, 10*time.Second)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("marshal session.load result: %w", err)
	}
	var parsed protocol.SessionLoadResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal session.load: %w", err)
	}
	return parsed.Messages, nil
}

// DeleteSession asks the sidecar to forget the given conversation.
func (c *ProcessClient) DeleteSession(id string) (bool, error) {
	params := protocol.SessionDeleteParams{SessionID: id}
	resp, err := c.call("session.delete", params, 5*time.Second)
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return false, fmt.Errorf("marshal session.delete result: %w", err)
	}
	var parsed protocol.SessionDeleteResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, fmt.Errorf("unmarshal session.delete: %w", err)
	}
	return parsed.Deleted, nil
}

// SendApproval forwards the user's approval to the sidecar.
func (c *ProcessClient) SendApproval(resp protocol.ApprovalResponse) error {
	_, err := c.call(protocol.MethodApprovalRespond, resp, 10*time.Second)
	return err
}

// HealthCheck pings the sidecar, verifying it is running and responsive.
func (c *ProcessClient) HealthCheck() error {
	// 90s — the sidecar is a bash wrapper that exec's `bun run
	// headless.ts`. First-spawn cost is dominated by bun's TS module
	// graph load (Core + MCP + tools + prompts) which on a cold FS
	// cache can hit 30-60s, especially under Python subprocess
	// harness (stdout piped, not terminal). Observed reproducer-03:
	// all 5 tasks failed at exactly 30s-33s with "rpc timeout after
	// 30s (method=ping)" — ping wasn't failing, banya-core just hadn't
	// finished init. Bumping to 90s is safe because chat.start has its
	// own per-turn timeout; HealthCheck only gates the first message.
	resp, err := c.call(protocol.MethodPing, nil, 90*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("ping failed: %s", resp.Error.Message)
	}
	return nil
}

// Close terminates the sidecar process and releases resources.
func (c *ProcessClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.mu.Lock()
	stdin := c.stdin
	cmd := c.cmd
	cancel := c.cancel
	c.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if cancel != nil {
				cancel()
			}
			<-done
		}
	}
	return nil
}

// BinPath returns the resolved sidecar binary path (for diagnostics).
func (c *ProcessClient) BinPath() string { return c.binPath }

// Events returns the receive-only event channel. Useful for adapters
// (e.g. server.Server) that want to consume events independently of
// SendMessage. The channel is created on first sidecar start; callers
// should invoke Start() (or any RPC call) first.
func (c *ProcessClient) Events() <-chan protocol.ServerEvent {
	if err := c.start(); err != nil {
		closed := make(chan protocol.ServerEvent)
		close(closed)
		return closed
	}
	return c.events
}

// Compile-time assertion.
var _ Client = (*ProcessClient)(nil)

// sentinel used by tests.
var errSidecarNotStarted = errors.New("sidecar not started")

var _ = errSidecarNotStarted
