// ClaudeCodeCritic — spawn the local Claude Code CLI and let it run
// the review as an isolated, tool-equipped session.
//
// Why Claude Code instead of the plain Anthropic Messages API?
//   Claude Code ships a built-in tool suite (Read / Bash / Grep) that
//   we'd otherwise have to reimplement on the critic side — runRuff,
//   runReproducer, runRelatedTests are all subprocess dispatches we
//   want to HAND OFF to the LLM rather than own. With Claude Code, the
//   critic is a self-sufficient reviewer: it can `Read` the patch,
//   `Bash(pytest …)` to verify, `Bash(python -c …)` to run the issue's
//   reproducer, and emit a JSON GapObject at the end.
//
// Transport:
//   claude -p --bare --print --output-format json --model <model>
//     --system-prompt <critic §1-§9>
//     --allowedTools "Read Bash(pytest *) Bash(ruff *) Bash(python *) Bash(git diff*)"
//     --disallowedTools "Edit Write WebFetch"
//     --add-dir <workspace>
//     --permission-mode bypassPermissions
//     --max-budget-usd 0.50
//     --setting-sources ""                   (no user/project settings)
//     "<review task: issue + patch + rubric>"
//
// Output shape:
//   Claude Code with --output-format json emits a single JSON object.
//   The shape varies slightly by version; we accept either:
//     { "result": "<raw text>", "cost_usd": ..., ... }           (--print)
//     { "type": "result", "subtype": "success", "result": "...", ... }
//   We extract the `result` field and hand it back to critic.go's
//   parseGapObject(), which already tolerates wrapped JSON blobs.

package critic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type ClaudeCodeProvider struct {
	// Path to the `claude` binary. Empty = resolved via PATH.
	BinPath string
	// Model alias (opus / sonnet / haiku) or full id (claude-sonnet-4-6).
	Model string
	// Dollar cap per review; forwarded as --max-budget-usd.
	MaxBudgetUSD float64
	// Workspace root the reviewer may read from (forwarded via --add-dir).
	// Usually the SWE-bench task's <workspace>/repo.
	AddDir string
	// Extra tool allowlist beyond the defaults (comma/space separated).
	ExtraAllowedTools string
	// Hard timeout; defaults to critic's defaultTimeout (120s).
	Timeout time.Duration
}

// NewClaudeCodeProvider builds a provider with sensible defaults.
// `model` may be "opus" / "sonnet" / a full model id; empty → "sonnet".
func NewClaudeCodeProvider(binPath, model string, timeout time.Duration) *ClaudeCodeProvider {
	if model == "" {
		model = "sonnet"
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	return &ClaudeCodeProvider{
		BinPath:      binPath,
		Model:        model,
		MaxBudgetUSD: 0.50,
		Timeout:      timeout,
	}
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

func (p *ClaudeCodeProvider) Review(ctx context.Context, args ReviewArgs) (string, error) {
	bin := p.BinPath
	if bin == "" {
		resolved, err := exec.LookPath("claude")
		if err != nil {
			return "", fmt.Errorf("claude-code: `claude` binary not on PATH (set BANYA_CRITIC_CLAUDE_BIN)")
		}
		bin = resolved
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("claude-code: binary not found: %s", bin)
	}

	addDir := args.RepoRoot
	if p.AddDir != "" {
		addDir = p.AddDir
	}

	allowedTools := "Read Bash(pytest *) Bash(ruff *) Bash(python *) Bash(python3 *) Bash(git diff *) Bash(git status *)"
	if p.ExtraAllowedTools != "" {
		allowedTools = allowedTools + " " + p.ExtraAllowedTools
	}
	// Block everything that could mutate the repo. `--disallowedTools`
	// is enforced server-side by Claude Code, so this is not just a
	// convention — violating tools genuinely will not execute.
	disallowedTools := "Edit Write WebFetch NotebookEdit MultiEdit"

	// The critic prompt we already built (§1-§9) IS the review task.
	// We pass it as the user turn; a compact system prompt just sets
	// identity so Claude Code doesn't default to its coding-assistant
	// persona.
	systemPrompt := strings.Join([]string{
		"You are the banya patch reviewer. Follow the user message's §1-§9",
		"protocol EXACTLY. Use Read / Bash tools to run the reproducer,",
		"pytest on related tests, and ruff as needed; do not modify any",
		"file. Emit ONLY the JSON GapObject defined in §8 — no markdown,",
		"no commentary outside JSON.",
	}, " ")

	cliCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	cliArgs := []string{
		"-p",
		"--bare",
		"--print",
		"--output-format", "json",
		"--model", p.Model,
		"--system-prompt", systemPrompt,
		"--allowedTools", allowedTools,
		"--disallowedTools", disallowedTools,
		"--permission-mode", "bypassPermissions",
		"--setting-sources", "",
		"--no-session-persistence",
	}
	if addDir != "" {
		// `--add-dir` accepts multiple values space-separated, but the
		// safe form is repeat-the-flag.
		cliArgs = append(cliArgs, "--add-dir", addDir)
	}
	if p.MaxBudgetUSD > 0 {
		cliArgs = append(cliArgs, "--max-budget-usd", fmt.Sprintf("%.2f", p.MaxBudgetUSD))
	}
	// Prompt comes last (positional).
	cliArgs = append(cliArgs, args.ReviewPrompt)

	cmd := exec.CommandContext(cliCtx, bin, cliArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf(
			"claude-code exec: %w — stderr: %s",
			err, truncate(stderr.String(), 400),
		)
	}
	return extractClaudeResult(stdout.Bytes())
}

// extractClaudeResult unwraps the envelope Claude Code's --output-format=json
// produces. Two shapes observed across versions:
//
//   { "result": "<model text>", "cost_usd": ..., "exit_reason": ... }
//   { "type": "result", "subtype": "success", "result": "...", ... }
//
// We also tolerate raw text (shouldn't happen with --output-format json
// but cheap to be defensive).
func extractClaudeResult(raw []byte) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("claude-code: empty stdout")
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// Fall back — maybe the model ignored the format flag.
		return string(raw), nil
	}
	if s, ok := envelope["result"].(string); ok {
		return s, nil
	}
	// Some versions nest the text inside a `content` / `message` field.
	if msg, ok := envelope["message"].(map[string]any); ok {
		if c, ok := msg["content"].(string); ok {
			return c, nil
		}
	}
	// As a last resort return the whole envelope — parseGapObject is
	// tolerant of leading prose.
	return string(raw), nil
}
