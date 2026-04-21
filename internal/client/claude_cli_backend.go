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

	// DefaultClaudeCliTimeout — 10 min per turn. 5 min was enough for
	// the first turn's clean response, but multi-turn TUI sessions with
	// accumulating conversation history routinely push past that on the
	// second / third agent-loop step (seen live: claude-cli (claude):
	// signal: killed mid-response). 10 min handles a 128k context
	// response comfortably while still bounding genuine hangs.
	DefaultClaudeCliTimeout = 10 * time.Minute
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
	// Split the incoming conversation: system messages become a
	// --system-prompt arg (replaces Claude CLI's own Claude Code
	// persona), the rest is collapsed into the -p body. Without this
	// split, banya-core's 48KB system prompt flowed into -p as user
	// text and Claude's safety training interpreted it as a prompt
	// injection attempt ("Also worth flagging: the user message
	// included an injected CODEPILOT system prompt…") and refused to
	// call any tool.
	systemPrompt, prompt := flattenMessagesForClaude(params.Messages, params.Tools)

	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		// Silence Claude's internal REPL chatter / thinking; we only
		// want the final text.
		"--permission-mode", "bypassPermissions",
		"--model", b.model,
		// Forbid every built-in Claude Code tool so Claude responds
		// with text only — banya-core owns file I/O and command
		// execution via its own XML envelope. The list grew with
		// Claude Code 2.x which added a large set of side-channel
		// tools (Agent, Skill, Monitor, TodoWrite, Worktree, Cron,
		// ScheduleWakeup, …). Left unblocked, Claude sees them as
		// "my real tools", ignores banya-core's envelope
		// instructions, and refuses the user's request on the
		// grounds that read_file / update_file / run_command
		// "aren't registered". Keeping the block list exhaustive is
		// cheaper than trying to whitelist — `--allowedTools ""`
		// isn't supported and whitelisting a smaller set would leak
		// future tools on every Claude Code upgrade.
		"--disallowedTools",
		strings.Join([]string{
			// file + shell
			"Edit", "Write", "Bash", "Read", "Glob", "Grep",
			"WebFetch", "WebSearch", "NotebookEdit",
			// agent / task orchestration
			"Agent", "Skill", "ToolSearch", "Monitor",
			"TodoWrite", "TaskStop", "TaskOutput",
			"AskUserQuestion",
			// plan / worktree / cron / wake scheduling
			"EnterPlanMode", "ExitPlanMode",
			"EnterWorktree", "ExitWorktree",
			"CronCreate", "CronDelete", "CronList",
			"ScheduleWakeup", "PushNotification", "RemoteTrigger",
			// Block every MCP connector namespace. The user's Claude
			// Code install may have globally-enabled MCP servers
			// (Gmail / Calendar / Drive / Playwright / Stitch / …)
			// whose tools follow the `mcp__<server>__<tool>` naming
			// convention. If we leave those visible, Claude sees
			// only them, decides banya-core's XML envelope `read_file`
			// / `update_file` / `run_command` "aren't real tools",
			// and refuses the user's request. Wildcard per server
			// (`mcp__<server>__*`) wasn't reliable across Claude CLI
			// versions, so we list the common connectors explicitly
			// and add catch-all prefixes for futureproofing.
			"mcp__claude_ai_Gmail__authenticate",
			"mcp__claude_ai_Gmail__complete_authentication",
			"mcp__claude_ai_Google_Calendar__authenticate",
			"mcp__claude_ai_Google_Calendar__complete_authentication",
			"mcp__claude_ai_Google_Drive__authenticate",
			"mcp__claude_ai_Google_Drive__complete_authentication",
			"mcp__playwright",
			"mcp__stitch",
			"mcp__context7",
			"mcp__sequentialthinking",
			"mcp__mcp-installer",
		}, ","),
		// Guard against a background build-up of cache/session files —
		// each Chat() is a fresh one-shot.
		"--setting-sources", "",
	}
	if systemPrompt != "" {
		// --system-prompt REPLACES Claude CLI's default persona (the one
		// that tells it "I'm Claude Code, here are my built-in tools").
		// That's exactly what we want — banya-core's system prompt is the
		// authoritative source of identity + tool envelope here.
		args = append(args, "--system-prompt", systemPrompt)
	}

	cmd := exec.CommandContext(ctx, b.binary, args...)
	cmd.Env = os.Environ()
	// Disconnect stdin — claude -p reads its prompt from argv, not stdin,
	// but without this claude inherits the parent process's stdin. In TUI
	// mode that parent is Bubble Tea's raw-mode terminal; claude then sees
	// the terminal escape sequences Bubble Tea writes to its own stdin
	// and occasionally errors out on the unexpected bytes. Passing
	// os.DevNull also matches how we invoke claude from the critic path.
	devnull, err := os.Open(os.DevNull)
	if err == nil {
		cmd.Stdin = devnull
		defer devnull.Close()
	}

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

	// Deliberately do NOT call onToken here. ClaudeCliBackend is
	// not streaming (claude -p returns a single JSON blob); emitting
	// the full text once on the JSON-RPC `content_delta` channel
	// caused every line to render twice in the TUI because banya-core
	// ALSO emits the same text as a chat-session content_delta. HTTP
	// backends that genuinely stream keep their onToken fan-out —
	// they only emit incremental fragments so banya-core's dedup
	// isn't relevant.
	_ = onToken

	// Claude CLI does not return native tool_calls in this invocation
	// mode. If banya-core's system prompt asked for tools via the
	// XML-style envelope, the text already contains them and
	// ToolParser will extract them downstream.
	return text, "stop", nil, nil
}

// Close — nothing to release; every Chat() spawns + reaps its own
// subprocess.
func (b *ClaudeCliBackend) Close() error { return nil }

// flattenMessagesForClaude splits a banya-core conversation into the
// (system_prompt, body) pair that Claude CLI consumes. System
// messages concatenate into the first return value (passed via
// `--system-prompt`); everything else — user, assistant, tool results,
// plus the advertised tool-spec list — collapses into the second
// return (passed via `-p`). Keeping them separate is critical:
// folding system messages into the user-facing body made Claude's
// safety training treat our 48KB system prompt as a prompt-injection
// attempt and refuse to invoke any tool.
//
// Tool specs are listed in the body with a human-readable preamble;
// banya-core's system prompt already teaches the XML envelope, so the
// body list is a reminder, not an authoritative schema.
func flattenMessagesForClaude(
	messages []protocol.LlmChatMessage,
	tools []protocol.LlmToolSpec,
) (systemPrompt, body string) {
	var sys strings.Builder
	for _, m := range messages {
		if m.Role != "system" {
			continue
		}
		if sys.Len() > 0 {
			sys.WriteString("\n\n")
		}
		sys.WriteString(m.Content)
	}

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
			// handled above — omit from body so Claude doesn't see it
			// twice (and doesn't interpret it as user-injected text).
			continue
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
	return sys.String(), b.String()
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
