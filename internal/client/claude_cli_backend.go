// ClaudeCliBackend — wraps the local `claude` CLI (Anthropic's Claude
// Code) as an LLMBackend. Invoked via `claude -p --output-format json
// --model <opus|sonnet|haiku>` per turn; Claude generates a complete
// assistant response (no streaming) and we surface it as `content`.
// Tool calls are carried inside the text using banya-core's native
// XML-style tool envelope, which its ToolParser already understands.
//
// Why not Anthropic's native Messages API?
//   Because the target deployment is Claude MAX subscription — no
//   per-token API cost. The `claude` CLI binary consumes that
//   subscription. Running a subprocess per turn is slower than an HTTP
//   streaming call but it keeps the cost model predictable and
//   subscription-only.
//
// Tool handling:
//   Claude Code's built-in Edit/Write/Bash would edit files directly
//   (bypassing banya-core's tool dispatch). We disable them via
//   --disallowedTools so Claude responds with text only; banya-core's
//   ToolParser then pulls the tool calls out. Read-only introspection
//   tools (Read, Glob, Grep) are also disabled because banya-core owns
//   the read path — we want one source of truth for "what files the
//   agent looked at".
//
// Streaming: Claude CLI's stream-json output is multi-event and
// designed for interactive TUIs. For the per-turn LLMBackend contract
// we use --output-format json (one-shot) and stream the full text to
// onToken in a single callback at the end. Downstream streamers
// (banya-core, tui) see the whole response at once; this is a minor
// UX regression vs native HTTP streaming but avoids a fragile
// stream-json parser.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

const (
	// DefaultClaudeCliModel — `sonnet` is the common balance. Callers
	// who want `opus` set it via BANYA_MAIN_MODEL or BackendConfig.Model.
	DefaultClaudeCliModel = "sonnet"

	// DefaultClaudeCliTimeout — 5 min per turn. Long enough for a 128k
	// context response, short enough that a hang doesn't block the
	// benchmark's own turn budget.
	DefaultClaudeCliTimeout = 5 * time.Minute
)

type ClaudeCliBackend struct {
	binary  string
	model   string
	timeout time.Duration
}

// NewClaudeCliBackend builds the adapter. Empty binary falls back to
// `$PATH` lookup of `claude`. Empty model falls back to
// DefaultClaudeCliModel.
func NewClaudeCliBackend(binary, model string) *ClaudeCliBackend {
	if binary == "" {
		binary = "claude"
	}
	if model == "" {
		model = DefaultClaudeCliModel
	}
	return &ClaudeCliBackend{
		binary:  binary,
		model:   model,
		timeout: DefaultClaudeCliTimeout,
	}
}

// Chat implements LLMBackend. Every call spawns one `claude -p`
// subprocess, feeds the concatenated conversation as the prompt, and
// returns the final assistant text. Token-level streaming is
// simulated by a single onToken invocation at end-of-turn.
func (b *ClaudeCliBackend) Chat(
	ctx context.Context,
	params protocol.LlmChatParams,
	onToken func(string) error,
) (string, string, []protocol.LlmToolCall, error) {
	prompt := flattenMessagesForClaude(params.Messages, params.Tools)

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		// Silence Claude's internal REPL chatter / thinking; we only
		// want the final text.
		"--permission-mode", "bypassPermissions",
		"--model", b.model,
		// Forbid every built-in tool so Claude responds with text only.
		// banya-core owns file I/O and command execution.
		"--disallowedTools", "Edit,Write,Bash,Read,Glob,Grep,WebFetch,WebSearch,NotebookEdit",
		// Guard against a background build-up of cache/session files —
		// each Chat() is a fresh one-shot.
		"--setting-sources", "",
	}

	cmd := exec.CommandContext(ctx, b.binary, args...)
	cmd.Env = os.Environ()

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if err != nil {
		// Surface stderr tail — most Claude CLI failures are "not
		// signed in" / quota / unknown model, and all of those are
		// actionable only if the user sees the exact message.
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
			if len(stderr) > 800 {
				stderr = "…" + stderr[len(stderr)-800:]
			}
		}
		return "", "", nil, fmt.Errorf(
			"claude-cli (%s): %w; stderr: %s", b.binary, err, stderr,
		)
	}

	text, parseErr := parseClaudeJsonOutput(out)
	if parseErr != nil {
		return "", "", nil, parseErr
	}

	// Optional trace: log the (messages, completion) pair for offline
	// LoRA training. Claude's native XML tool envelope IS exactly what
	// banya-core's ToolParser expects, so these raw I/O pairs are
	// tokenisable directly as SFT data for a Qwen adapter that learns
	// the same format. Set BANYA_LLM_TRACE_PATH to a directory to
	// enable; silent no-op if unset.
	if trace := os.Getenv("BANYA_LLM_TRACE_PATH"); trace != "" {
		appendTrace(trace, params, text, b.model, elapsed)
	}

	// Simulate streaming: a single callback at end-of-turn. banya-core
	// accumulates onToken fragments into content; passing the full
	// text once produces the same final state as a real stream.
	if onToken != nil {
		if err := onToken(text); err != nil {
			// onToken may cancel the stream (e.g. ctx cancel). We've
			// already produced the full response, so propagate err
			// without discarding the content.
			return text, "stop", nil, err
		}
	}

	// Claude CLI does not return native tool_calls in this invocation
	// mode. If banya-core's system prompt asked for tools via the
	// XML-style envelope, the text already contains them and
	// ToolParser will extract them downstream.
	return text, "stop", nil, nil
}

// Close — nothing to release; every Chat() spawns + reaps its own
// subprocess.
func (b *ClaudeCliBackend) Close() error { return nil }

// flattenMessagesForClaude collapses a message list + tool spec into a
// single prompt string. Claude CLI takes a -p string; we prefix
// system messages with "# System", others with role labels.
func flattenMessagesForClaude(
	messages []protocol.LlmChatMessage,
	tools []protocol.LlmToolSpec,
) string {
	var b strings.Builder
	if len(tools) > 0 {
		b.WriteString("# Available tools (reply in banya-core's XML envelope to invoke):\n")
		for _, t := range tools {
			b.WriteString("- ")
			b.WriteString(t.Function.Name)
			if t.Function.Description != "" {
				b.WriteString(" — ")
				b.WriteString(t.Function.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	for _, m := range messages {
		switch m.Role {
		case "system":
			b.WriteString("# System\n")
		case "user":
			b.WriteString("# User\n")
		case "assistant":
			b.WriteString("# Assistant\n")
		case "tool":
			b.WriteString("# Tool result\n")
		default:
			b.WriteString("# ")
			b.WriteString(string(m.Role))
			b.WriteString("\n")
		}
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}
	return b.String()
}

// parseClaudeJsonOutput extracts the text payload from the one-shot
// JSON envelope `claude -p --output-format json` emits. Two common
// shapes:
//
//	{"type": "result", "subtype": "success", "result": "<text>", ...}
//	{"type": "result", "result": "<text>"}                  (older builds)
//
// Rather than enumerate every variant, we unmarshal into a loose map
// and pluck `result`. Absence is a hard error — no text = no response.
func parseClaudeJsonOutput(raw []byte) (string, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		// Some builds emit a bare string when the assistant's reply
		// is pure text — accept that as a fallback.
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, nil
		}
		return "", fmt.Errorf("claude-cli: cannot parse output as JSON: %w", err)
	}
	if r, ok := obj["result"].(string); ok {
		return r, nil
	}
	return "", fmt.Errorf(
		"claude-cli: no 'result' field in output (keys: %v)", mapKeys(obj),
	)
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// init registers this adapter in the shared factory registry. Doing
// it here (not in the registry file) keeps the registry independent
// of each adapter's imports — new adapters just need their own file.
func init() {
	Register("claude-cli", func(cfg BackendConfig) (LLMBackend, error) {
		return NewClaudeCliBackend(cfg.BinaryPath, cfg.Model), nil
	})
}
