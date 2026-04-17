package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/spf13/cobra"
)

// banya run ─ headless batch mode.
//
// Intended for evaluation harnesses and other non-TUI callers. Spawns the
// sidecar, sends one chat.start turn, streams every ServerEvent as one-line
// NDJSON on stdout, and exits when the turn completes. Sidecar stderr is
// passed through to this process's stderr unchanged — callers can tee it.
//
// Output events on stdout:
//
//	{"type":"<event_type>","session_id":"...","data":{...}}\n
//
// Plus two harness-only markers (not from the sidecar):
//
//	{"type":"meta","data":{"phase":"start","session_id":"...","elapsed_ms":0}}
//	{"type":"meta","data":{"phase":"exit","reason":"done|idle_abort|timeout|error","elapsed_ms":N}}
//
// Exit codes:
//
//	0  normal "done" event received
//	1  invocation / config error
//	2  agent timeout or idle abort
//	3  sidecar RPC / protocol error

func runCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run one chat turn headlessly, streaming ServerEvents as NDJSON",
		Long: `run spawns a banya-core sidecar, sends a single chat.start, and
emits every ServerEvent on stdout as NDJSON (one JSON object per line).
Intended for eval harnesses that drive the agent programmatically.`,
		RunE: runHeadless,
	}
	cmd.Flags().String("prompt", "", "Prompt text (use --prompt-file for multi-line)")
	cmd.Flags().String("prompt-file", "", "Read prompt text from this file ('-' for stdin)")
	cmd.Flags().String("workspace", "", "Working directory for the agent (default: cwd)")
	cmd.Flags().String("prompt-type", "agent", "Prompt type: code|ask|plan|agent")
	cmd.Flags().Duration("timeout", 600*time.Second, "Hard timeout for the turn")
	cmd.Flags().Duration("idle-abort", 180*time.Second, "Abort if no tool call within this duration after start (0 disables)")
	cmd.Flags().Bool("auto-approve", true, "Auto-approve every approval_needed event")
	return cmd
}

func runHeadless(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Honor root-level flags (sidecar path, llm url/key/model/target-port).
	if sc, _ := cmd.Root().Flags().GetString("sidecar"); sc != "" {
		cfg.Sidecar.Path = sc
	}
	if u, _ := cmd.Root().Flags().GetString("llm-url"); u != "" {
		cfg.LLMServer.URL = u
	}
	if k, _ := cmd.Root().Flags().GetString("llm-key"); k != "" {
		cfg.LLMServer.APIKey = k
	}
	if m, _ := cmd.Root().Flags().GetString("llm-model"); m != "" {
		cfg.LLMServer.Model = m
	}
	if tp, _ := cmd.Root().Flags().GetString("llm-target-port"); tp != "" {
		cfg.LLMServer.TargetPort = tp
	}

	promptText, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}
	workspace, _ := cmd.Flags().GetString("workspace")
	if workspace != "" {
		if err := os.Chdir(workspace); err != nil {
			return fmt.Errorf("chdir workspace: %w", err)
		}
	}
	promptTypeStr, _ := cmd.Flags().GetString("prompt-type")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	idleAbort, _ := cmd.Flags().GetDuration("idle-abort")
	autoApprove, _ := cmd.Flags().GetBool("auto-approve")

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	start := time.Now()
	emitMeta(out, map[string]any{
		"phase":      "start",
		"elapsed_ms": 0,
		"sidecar":    cfg.Sidecar.Path,
		"llm_url":    cfg.LLMServer.URL,
		"llm_target": cfg.LLMServer.TargetPort,
	})

	pc, err := client.NewProcessClient(cfg.Sidecar.Path)
	if err != nil {
		emitMeta(out, map[string]any{"phase": "exit", "reason": "sidecar_spawn_error", "error": err.Error()})
		return fmt.Errorf("init sidecar: %w", err)
	}
	defer pc.Close()
	pc.SetLLMBackend(client.NewLLMServerClientWithTarget(
		cfg.LLMServer.URL, cfg.LLMServer.APIKey, cfg.LLMServer.Model, cfg.LLMServer.TargetPort,
	))

	if err := pc.HealthCheck(); err != nil {
		emitMeta(out, map[string]any{"phase": "exit", "reason": "sidecar_health", "error": err.Error()})
		return fmt.Errorf("sidecar health check failed: %w", err)
	}

	req := protocol.ChatRequest{
		Message:    promptText,
		PromptType: protocol.PromptType(promptTypeStr),
	}
	if wd, wderr := os.Getwd(); wderr == nil {
		req.WorkDir = wd
	}

	events, err := pc.SendMessage(req)
	if err != nil {
		emitMeta(out, map[string]any{"phase": "exit", "reason": "chat_start_error", "error": err.Error()})
		return fmt.Errorf("chat.start: %w", err)
	}

	// Drive the event loop with overall + idle-abort deadlines.
	hardDeadline := time.After(timeout)
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if idleAbort > 0 {
		idleTimer = time.NewTimer(idleAbort)
		idleCh = idleTimer.C
	}
	sawTool := false
	sessionID := ""
	exitReason := "done"
	exitCode := 0

loop:
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				exitReason = "channel_closed"
				exitCode = 3
				break loop
			}
			if evt.SessionID != "" && sessionID == "" {
				sessionID = evt.SessionID
			}
			if err := writeEvent(out, evt); err != nil {
				exitReason = "stdout_write_error"
				exitCode = 3
				break loop
			}
			switch evt.Type {
			case protocol.EventToolCallStart:
				sawTool = true
				if idleTimer != nil {
					idleTimer.Stop()
				}
			case protocol.EventApprovalNeeded:
				if autoApprove {
					id := extractStringField(evt.Data, "tool_call_id")
					if id != "" {
						_ = pc.SendApproval(protocol.ApprovalResponse{
							SessionID:  sessionID,
							ToolCallID: id,
							Approved:   true,
						})
					}
				}
			case protocol.EventDone:
				break loop
			case protocol.EventError:
				// Report and exit with code 3 — sidecar-level error.
				exitReason = "sidecar_error"
				exitCode = 3
				break loop
			}
		case <-hardDeadline:
			exitReason = "timeout"
			exitCode = 2
			break loop
		case <-idleCh:
			if !sawTool {
				exitReason = "idle_abort"
				exitCode = 2
				break loop
			}
		}
	}

	emitMeta(out, map[string]any{
		"phase":      "exit",
		"reason":     exitReason,
		"session_id": sessionID,
		"elapsed_ms": time.Since(start).Milliseconds(),
	})
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

// resolvePrompt reads --prompt, --prompt-file, or stdin.
func resolvePrompt(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("prompt"); p != "" {
		return p, nil
	}
	pf, _ := cmd.Flags().GetString("prompt-file")
	if pf == "" {
		return "", fmt.Errorf("one of --prompt or --prompt-file is required")
	}
	if pf == "-" {
		data, err := os.ReadFile("/dev/stdin")
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(pf)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", pf, err)
	}
	return string(data), nil
}

func writeEvent(w *bufio.Writer, evt protocol.ServerEvent) error {
	b, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

func emitMeta(w *bufio.Writer, data map[string]any) {
	msg := map[string]any{"type": "meta", "data": data}
	b, _ := json.Marshal(msg)
	_, _ = w.Write(b)
	_ = w.WriteByte('\n')
	_ = w.Flush()
}

// extractStringField pulls `field` out of either a map[string]any (post-JSON)
// or a struct via encoding/json round-trip.
func extractStringField(v any, field string) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m[field].(string); ok {
			return s
		}
		return ""
	}
	// Fallback: marshal then lookup.
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	if s, ok := m[field].(string); ok {
		return s
	}
	return ""
}
