package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/internal/critic"
	"github.com/cascadecodes/banya-cli/internal/domain"
	"github.com/cascadecodes/banya-cli/internal/webbench"
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
	cmd.Flags().Bool("critic", true, "Run a Gemini reviewer on patch.diff after the agent finishes; if REVISE, send one follow-up turn")
	cmd.Flags().String("critic-issue-file", "", "Optional path to the issue/problem statement text the critic should compare the patch against. Defaults to the prompt itself when omitted.")
	cmd.Flags().Bool("no-patch-nudge", false, "Disable the post-first-turn nudge that forces the agent to commit to a patch when patch.diff is missing.")
	cmd.Flags().Duration("nudge-timeout", 300*time.Second, "Hard timeout for the nudge turn (shorter than the main timeout; nudge is commit-only work).")
	cmd.Flags().Int("critic-max-rounds", 3, "Maximum number of critic→revise rounds. Stops early on critic OK or unchanged patch.")
	cmd.Flags().Duration("thinking-abort", 300*time.Second, "Abort a turn when the agent goes this long between tool calls after the first one (catches thinking-only loops). 0 disables.")
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
	criticEnabled, _ := cmd.Flags().GetBool("critic")
	criticIssueFile, _ := cmd.Flags().GetString("critic-issue-file")
	noPatchNudge, _ := cmd.Flags().GetBool("no-patch-nudge")
	nudgeTimeout, _ := cmd.Flags().GetDuration("nudge-timeout")
	criticMaxRounds, _ := cmd.Flags().GetInt("critic-max-rounds")
	if criticMaxRounds < 1 {
		criticMaxRounds = 1
	}
	thinkingAbort, _ := cmd.Flags().GetDuration("thinking-abort")

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

	// Domain classification — deterministic static file scan. Emits a
	// `domain_match` meta event AND scales the runtime budget so OOD /
	// unvalidated tasks get extra investigation time and an extra
	// critic round before we give up. The scan result is also threaded
	// into the sidecar env so banya-core's prompt can self-narrate the
	// confidence tier (Layer 3 of the domain stack).
	var domScan domain.ScanResult
	if scanRoot := resolveDomainScanRoot(cmd); scanRoot != "" {
		domScan = domain.Scan(scanRoot)
		payload := map[string]any{
			"phase":       "domain_match",
			"scan_root":   scanRoot,
			"tier":        domScan.Tier,
			"total_files": domScan.Sig.TotalFiles,
		}
		if domScan.Top != nil {
			payload["top_key"] = domScan.Top.Domain.Key
			payload["top_label"] = domScan.Top.Domain.Label
			payload["top_score"] = domScan.Top.Score
			payload["validated"] = domScan.Top.Domain.HasValidation()
			if domScan.Top.Domain.HasValidation() {
				payload["best_pass_rate"] = domScan.Top.Domain.BestPassRate()
			}
		}
		ranked := make([]map[string]any, 0, len(domScan.Ranked))
		for i, r := range domScan.Ranked {
			if i >= 5 {
				break
			}
			ranked = append(ranked, map[string]any{
				"key":       r.Domain.Key,
				"score":     r.Score,
				"validated": r.Domain.HasValidation(),
			})
		}
		payload["ranked"] = ranked
		emitMeta(out, payload)

		// Human-readable stderr warning for low/none tiers so an
		// interactive user sees the OOD signal without having to
		// parse the NDJSON event stream.
		switch domScan.Tier {
		case "none":
			fmt.Fprintf(os.Stderr,
				"[banya-cli] ⚠ workspace does not match any benchmark-validated domain. "+
					"Agent capability on this task is untested — expect lower reliability than our SWE-bench numbers suggest.\n")
		case "low":
			if domScan.Top != nil {
				fmt.Fprintf(os.Stderr,
					"[banya-cli] ⚠ weak domain match (%s, score %.2f) — task is adjacent to but outside our benchmark coverage. "+
						"Reliability is uncertain.\n",
					domScan.Top.Domain.Label, domScan.Top.Score)
			}
		case "medium":
			if domScan.Top != nil && !domScan.Top.Domain.HasValidation() {
				fmt.Fprintf(os.Stderr,
					"[banya-cli] ⓘ workspace looks like %s, but we have no benchmark measurement for that domain yet. "+
						"Treat quality as unverified.\n",
					domScan.Top.Domain.Label)
			}
		}

		// Layer 3a — scale runtime budget when confidence is low.
		// Rationale: a workspace we have no benchmark for needs more
		// investigation slack and one extra critic round to compensate
		// for the lack of priors. The multipliers are conservative —
		// validated/high-tier work is unaffected.
		timeout, idleAbort, criticMaxRounds = scaleBudgetForTier(
			domScan.Tier,
			domScan.Top != nil && domScan.Top.Domain.HasValidation(),
			timeout, idleAbort, criticMaxRounds,
		)
		nudgeTimeout = scaleNudgeTimeoutForTier(domScan.Tier, nudgeTimeout)

		emitMeta(out, map[string]any{
			"phase":             "domain_budget",
			"tier":              domScan.Tier,
			"timeout_s":         int(timeout.Seconds()),
			"idle_abort_s":      int(idleAbort.Seconds()),
			"nudge_timeout_s":   int(nudgeTimeout.Seconds()),
			"critic_max_rounds": criticMaxRounds,
		})
	}

	pc, err := client.NewProcessClient(cfg.Sidecar.Path)
	if err != nil {
		emitMeta(out, map[string]any{"phase": "exit", "reason": "sidecar_spawn_error", "error": err.Error()})
		return fmt.Errorf("init sidecar: %w", err)
	}
	defer pc.Close()
	// Backend selection: `--llm-backend` flag (cmd.Root) > BANYA_MAIN_PROVIDER
	// env > legacy default of llm-server (on-prem Qwen stack). The
	// concrete construction lives in internal/client/backend_registry.go
	// so a new adapter = one file + one init() call, not another case
	// branch here.
	backendKind, _ := cmd.Root().Flags().GetString("llm-backend")
	bcfg := client.ResolveBackendConfig()
	if backendKind != "" {
		bcfg.Provider = backendKind
	}
	// For the llm-server path, flag-fed CLI args (--llm-url / --llm-key /
	// --llm-model / --llm-target-port) carry what the harness passes.
	// Layer them onto the config only when env didn't already win —
	// gemini / claude-cli paths deliberately ignore these because the
	// harness hard-codes the vLLM model id and URL.
	canonical := strings.ToLower(bcfg.Provider)
	if canonical == "" || canonical == "llm-server" || canonical == "vllm" {
		if bcfg.Endpoint == "" {
			bcfg.Endpoint = cfg.LLMServer.URL
		}
		if bcfg.APIKey == "" {
			bcfg.APIKey = cfg.LLMServer.APIKey
		}
		if bcfg.Model == "" {
			bcfg.Model = cfg.LLMServer.Model
		}
		if bcfg.TargetPort == "" {
			bcfg.TargetPort = cfg.LLMServer.TargetPort
		}
	} else if bcfg.APIKey == "" {
		// Gemini / claude-cli: fall back to --llm-key only if env
		// really empty (historical callers set the key via the flag).
		if k, _ := cmd.Root().Flags().GetString("llm-key"); k != "" {
			bcfg.APIKey = k
		}
	}
	backend, err := client.NewBackendFromConfig(bcfg)
	if err != nil {
		return fmt.Errorf("main-agent backend: %w", err)
	}
	pc.SetLLMBackend(backend)
	env := client.SubagentEnvVars(cfg.Subagent.Provider, cfg.Subagent.Model, cfg.Subagent.APIKey, cfg.Subagent.Endpoint)
	if v := client.LanguageEnvVar(cfg.Language); v != "" {
		env = append(env, v)
	}
	// Layer 3b — propagate domain classification so banya-core's
	// PromptComposer can inject a self-aware "you are operating in
	// {label} (tier: {tier})" section. Empty values are skipped so
	// non-classified runs don't pollute the env.
	if vs := domainEnvVars(domScan); len(vs) > 0 {
		env = append(env, vs...)
	}
	if len(env) > 0 {
		pc.SetExtraEnv(env)
	}

	if err := pc.HealthCheck(); err != nil {
		emitMeta(out, map[string]any{"phase": "exit", "reason": "sidecar_health", "error": err.Error()})
		return fmt.Errorf("sidecar health check failed: %w", err)
	}

	workDir, _ := os.Getwd()

	// First turn — the agent's initial attempt.
	sessionID, exitReason, exitCode := runOneTurn(out, pc, protocol.ChatRequest{
		Message:    promptText,
		WorkDir:    workDir,
		PromptType: protocol.PromptType(promptTypeStr),
	}, timeout, idleAbort, thinkingAbort, autoApprove)
	refreshPatchDiff(workDir, out)

	patchPath := filepath.Join(workDir, "patch.diff")
	patchExists := func() bool {
		st, err := os.Stat(patchPath)
		return err == nil && st.Size() > 0
	}

	// 1-shot nudge guard. v6-strat logs showed sklearn-10297 had the
	// "STOP…patch.diff" phrase appearing 9 times in its trajectory —
	// investigation found the phrase also leaks through revise-turn
	// feedback and system prompts, so we gate firing explicitly even
	// though the code path is already single-entry. Set-and-forget:
	// once the flag is true we skip the nudge block for the rest of
	// the run even if a later revise turn drops the patch again.
	nudgedThisRun := false

	// Webbench-aware path — parallel to the SWE-bench patch.diff flow
	// below. Detects `test/task-N.spec.js` + `playwright.config.*` and
	// runs the same cumulative test command Web-Bench's official
	// evaluator does (`npx playwright test test/task-1.spec.js …
	// test/task-N.spec.js`). On any failure we fire a single nudge
	// turn that hands the test output back to the agent. The two
	// layouts are mutually exclusive in practice (a SWE-bench repo has
	// `repo/` and no task specs, and vice versa), but we gate the
	// SWE-bench blocks on `!wbLayout.Active()` below to make it
	// explicit.
	wbLayout := webbench.Detect(workDir)
	if wbLayout.Active() && !noPatchNudge && exitCode != 3 {
		emitMeta(out, map[string]any{
			"phase":         "webbench_probe",
			"layout":        wbLayout.DescribeShort(),
			"current_index": wbLayout.CurrentIndex,
			"session_id":    sessionID,
		})
		// Cumulative range goes up to the current task index (set by
		// the orchestrator via BANYA_WEBBENCH_CURRENT_INDEX), not the
		// full spec count. Earlier iterations scoped to MaxIndex and
		// fed the agent 20 nominal failures on a task-1 attempt; the
		// agent then tried to solve all tasks at once (80+ tool calls,
		// 540s turns). Gating to CurrentIndex matches Web-Bench's
		// official `npm run test -- N` semantics.
		specs := wbLayout.SpecRange(wbLayout.CurrentIndex)
		wbCtx, wbCancel := context.WithTimeout(cmd.Context(), 12*time.Minute)
		tres, terr := webbench.RunCumulative(wbCtx, workDir, specs, 10*time.Minute)
		wbCancel()
		emitMeta(out, map[string]any{
			"phase":        "webbench_test",
			"all_passed":   tres.AllPassed,
			"passed":       tres.PassedCount,
			"failed":       tres.FailedCount,
			"failed_specs": tres.FailedSpecs,
			"duration_ms":  tres.Duration.Milliseconds(),
			"timed_out":    tres.TimedOut,
			"exit_code":    tres.ExitCode,
			"error":        errString(terr),
			"session_id":   sessionID,
		})
		if !tres.AllPassed && !nudgedThisRun && terr == nil {
			nudgedThisRun = true
			emitMeta(out, map[string]any{
				"phase":      "webbench_nudge",
				"prev_exit":  exitReason,
				"session_id": sessionID,
			})
			_, exitReason, exitCode = runOneTurn(out, pc, protocol.ChatRequest{
				SessionID:  sessionID,
				Message:    webbench.BuildNudge(tres, len(specs) > 1),
				WorkDir:    workDir,
				PromptType: protocol.PromptType(promptTypeStr),
			}, nudgeTimeout, idleAbort, thinkingAbort, autoApprove)
			// Re-run tests so eval harnesses can record the post-nudge
			// state without re-invoking playwright themselves.
			wbCtx2, wbCancel2 := context.WithTimeout(cmd.Context(), 12*time.Minute)
			tres2, _ := webbench.RunCumulative(wbCtx2, workDir, specs, 10*time.Minute)
			wbCancel2()
			emitMeta(out, map[string]any{
				"phase":        "webbench_test_after_nudge",
				"all_passed":   tres2.AllPassed,
				"passed":       tres2.PassedCount,
				"failed":       tres2.FailedCount,
				"failed_specs": tres2.FailedSpecs,
				"duration_ms":  tres2.Duration.Milliseconds(),
				"session_id":   sessionID,
			})
		}
	}

	// Post-hoc nudge: the first turn ended without writing patch.diff.
	// Agents failing this way tend to fall into two modes — they quit
	// mid-investigation (tools=3, fast), or they loop on read_file /
	// ast_search until they hit the hard timeout (tools=24, 900s). Both
	// modes walk away with zero points even when the critic could
	// rescue a wrong patch. Send a single "commit now" continuation
	// turn with a tighter budget so the agent can lock in ANY patch.
	if !wbLayout.Active() && !noPatchNudge && !nudgedThisRun && exitCode != 3 && !patchExists() {
		nudgedThisRun = true
		emitMeta(out, map[string]any{
			"phase":      "nudge",
			"prev_exit":  exitReason,
			"session_id": sessionID,
		})
		_, exitReason, exitCode = runOneTurn(out, pc, protocol.ChatRequest{
			SessionID:  sessionID, // continue the same conversation
			Message:    buildNudgePrompt(),
			WorkDir:    workDir,
			PromptType: protocol.PromptType(promptTypeStr),
		}, nudgeTimeout, idleAbort, thinkingAbort, autoApprove)
		refreshPatchDiff(workDir, out)
	}

	// Critic phase — up to `criticMaxRounds` review→revise cycles. Stops
	// early on: (a) critic OK, (b) revise turn errored, (c) patch did not
	// change between rounds (agent stuck). Gated on non-webbench layout
	// so the Web-Bench run doesn't try to review a nonexistent patch.diff.
	if !wbLayout.Active() && criticEnabled && exitCode == 0 && patchExists() {
		reviewer := critic.NewFromEnv()
		if reviewer != nil {
			issueText, _ := readIssueForCritic(criticIssueFile, promptText)
			var lastPatchHash string
			for round := 1; round <= criticMaxRounds; round++ {
				patchBytes, _ := os.ReadFile(patchPath)
				hash := fmt.Sprintf("%d-%d", len(patchBytes), hashBytes(patchBytes))
				if round > 1 && hash == lastPatchHash {
					emitMeta(out, map[string]any{
						"phase":      "critic_revise_stuck",
						"round":      round,
						"session_id": sessionID,
					})
					break
				}
				lastPatchHash = hash

				ctx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
				decision, cerr := reviewer.ReviewPatch(ctx, issueText, string(patchBytes), workDir)
				cancel()
				if cerr != nil {
					emitMeta(out, map[string]any{
						"phase":      "critic_error",
						"round":      round,
						"error":      cerr.Error(),
						"session_id": sessionID,
					})
					break
				}
				emitMeta(out, map[string]any{
					"phase":      "critic",
					"round":      round,
					"ok":         decision.OK,
					"feedback":   decision.Feedback,
					"session_id": sessionID,
				})
				if decision.OK {
					break
				}
				if round == criticMaxRounds {
					emitMeta(out, map[string]any{
						"phase":      "critic_revise_budget_exhausted",
						"round":      round,
						"session_id": sessionID,
					})
					break
				}
				// REVISE turn.
				revisePrompt := buildRevisePrompt(decision.Feedback)
				_, exitReason, exitCode = runOneTurn(out, pc, protocol.ChatRequest{
					SessionID:  sessionID,
					Message:    revisePrompt,
					WorkDir:    workDir,
					PromptType: protocol.PromptType(promptTypeStr),
				}, timeout, idleAbort, thinkingAbort, autoApprove)
				refreshPatchDiff(workDir, out)
				if exitCode != 0 || !patchExists() {
					break
				}
			}
		}
	} else if !wbLayout.Active() && criticEnabled && exitCode == 0 && !patchExists() {
		emitMeta(out, map[string]any{
			"phase":  "critic_skip",
			"reason": "no patch.diff (even after nudge)",
		})
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

// runOneTurn drives a single chat.start cycle from SendMessage to EventDone
// (or timeout / abort). Extracted so the critic loop can call it twice.
// Returns the session_id seen, the exit reason string, and the exit code
// (0 = clean done, 2 = timeout / idle, 3 = protocol / sidecar error).
func runOneTurn(
	out *bufio.Writer,
	pc *client.ProcessClient,
	req protocol.ChatRequest,
	timeout, idleAbort, thinkingAbort time.Duration,
	autoApprove bool,
) (sessionID, exitReason string, exitCode int) {
	events, err := pc.SendMessage(req)
	if err != nil {
		emitMeta(out, map[string]any{"phase": "chat_error", "error": err.Error()})
		return "", "chat_start_error", 3
	}

	hardDeadline := time.After(timeout)
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if idleAbort > 0 {
		idleTimer = time.NewTimer(idleAbort)
		idleCh = idleTimer.C
	}
	// Thinking-only abort: after the agent's FIRST tool call we arm a
	// new timer that resets on every subsequent tool call. If no new
	// tool call arrives for `thinkingAbort`, we assume the LLM is
	// stuck in a <think> loop (sympy-11400 in v6-strat spent 19 min
	// this way with just 1 tool call) and bail out.
	var thinkingTimer *time.Timer
	var thinkingCh <-chan time.Time
	sawTool := false
	exitReason = "done"

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
				if thinkingAbort > 0 {
					if thinkingTimer == nil {
						thinkingTimer = time.NewTimer(thinkingAbort)
						thinkingCh = thinkingTimer.C
					} else {
						if !thinkingTimer.Stop() {
							// Drain stale tick if any, before reset.
							select {
							case <-thinkingTimer.C:
							default:
							}
						}
						thinkingTimer.Reset(thinkingAbort)
					}
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
		case <-thinkingCh:
			// Only fires after sawTool became true — see case above.
			exitReason = "thinking_abort"
			exitCode = 2
			break loop
		}
	}
	return sessionID, exitReason, exitCode
}

// readIssueForCritic returns the text the critic should compare the patch
// against. Prefer an explicit --critic-issue-file; else fall back to the
// original prompt (which for SWE-bench includes the issue verbatim).
func readIssueForCritic(path, fallback string) (string, error) {
	if path == "" {
		return fallback, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fallback, err
	}
	return string(b), nil
}

// domainEnvVars renders the domain scan as KEY=VALUE pairs that
// banya-core can read at startup. The core's PromptComposer turns
// these into a "Domain awareness" section so the agent self-narrates
// confidence ("you're in python_web; we measured ~33% pass here").
//
// Empty / no-match scans yield an empty slice so we don't push noise.
func domainEnvVars(s domain.ScanResult) []string {
	if s.Top == nil && s.Tier == "" {
		return nil
	}
	out := []string{}
	if s.Tier != "" {
		out = append(out, "BANYA_DOMAIN_TIER="+s.Tier)
	}
	if s.Top != nil {
		out = append(out,
			"BANYA_DOMAIN_KEY="+s.Top.Domain.Key,
			"BANYA_DOMAIN_LABEL="+s.Top.Domain.Label,
			"BANYA_DOMAIN_DESCRIPTION="+s.Top.Domain.Description,
			fmt.Sprintf("BANYA_DOMAIN_SCORE=%.2f", s.Top.Score),
		)
		if s.Top.Domain.HasValidation() {
			out = append(out,
				"BANYA_DOMAIN_VALIDATED=1",
				fmt.Sprintf("BANYA_DOMAIN_PASS_RATE=%.2f", s.Top.Domain.BestPassRate()),
			)
		} else {
			out = append(out, "BANYA_DOMAIN_VALIDATED=0")
		}
	}
	return out
}

// scaleBudgetForTier widens timeout / idle-abort and bumps the critic
// retry ceiling for tasks outside our benchmark-validated coverage.
// Tiers and multipliers were chosen so:
//   - "high" + validated: no change (we trust the prior).
//   - "medium" + validated: no change.
//   - "medium" + unvalidated: +1 critic round; budgets unchanged.
//   - "low": +1 critic round, idle x1.5, timeout x1.3.
//   - "none": +1 critic round, idle x1.5, timeout x1.5.
// Conservative on purpose — overspending on every untested workspace
// would tank latency for the common case.
func scaleBudgetForTier(
	tier string,
	validated bool,
	timeout, idle time.Duration,
	criticRounds int,
) (time.Duration, time.Duration, int) {
	switch tier {
	case "none":
		return mulDur(timeout, 1.5), mulDur(idle, 1.5), criticRounds + 1
	case "low":
		return mulDur(timeout, 1.3), mulDur(idle, 1.5), criticRounds + 1
	case "medium":
		if !validated {
			return timeout, idle, criticRounds + 1
		}
		return timeout, idle, criticRounds
	}
	// "high" or unknown — leave as-is.
	return timeout, idle, criticRounds
}

func scaleNudgeTimeoutForTier(tier string, nudge time.Duration) time.Duration {
	switch tier {
	case "none":
		return mulDur(nudge, 1.5)
	case "low":
		return mulDur(nudge, 1.3)
	}
	return nudge
}

func mulDur(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * f)
}

// firstNonEmpty returns the first argument that is not an empty string.
// Used to cascade through BANYA_MAIN_* → legacy → hardcoded defaults
// when wiring up LLM backends.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// resolveDomainScanRoot picks the best directory to classify for the
// domain_match signal. Priority:
//   1. `--workspace` flag, when set (SWE-bench layout: scan `<ws>/repo`
//      if it exists, else `<ws>` itself).
//   2. process cwd as a fallback.
// Returns "" when no usable directory exists — callers skip the scan.
func resolveDomainScanRoot(cmd *cobra.Command) string {
	if ws, _ := cmd.Flags().GetString("workspace"); ws != "" {
		if st, err := os.Stat(filepath.Join(ws, "repo")); err == nil && st.IsDir() {
			return filepath.Join(ws, "repo")
		}
		if st, err := os.Stat(ws); err == nil && st.IsDir() {
			return ws
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// refreshPatchDiff regenerates `<workDir>/patch.diff` from the current
// `<workDir>/repo` working tree. The agent is supposed to do this itself
// via `run_command`, but in practice it often forgets or gets
// interrupted mid-turn — leaving a missing/stale patch.diff even when
// update_file already applied the fix. Calling this hook at every turn
// boundary closes that gap cheaply.
//
// No-op when `repo/` is absent (non-SWE-bench workspaces) or when git
// itself fails. Errors are intentionally swallowed: we never want this
// hook to block the main run loop.
func refreshPatchDiff(workDir string, out *bufio.Writer) {
	repoDir := filepath.Join(workDir, "repo")
	if st, err := os.Stat(repoDir); err != nil || !st.IsDir() {
		emitMeta(out, map[string]any{"phase": "patch_refresh_skip", "reason": "no_repo_dir"})
		return
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		emitMeta(out, map[string]any{"phase": "patch_refresh_skip", "reason": "no_git"})
		return
	}
	// git add -A; diff --cached; restore --staged .
	addCtx, addCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer addCancel()
	if err := execCmd(addCtx, repoDir, "git", "add", "-A"); err != nil {
		emitMeta(out, map[string]any{"phase": "patch_refresh_error", "step": "add", "error": err.Error()})
		return
	}
	diffCtx, diffCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer diffCancel()
	diffOut, err := execCmdOutput(diffCtx, repoDir, "git", "diff", "--cached")
	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer restoreCancel()
	_ = execCmd(restoreCtx, repoDir, "git", "restore", "--staged", ".")
	if err != nil {
		emitMeta(out, map[string]any{"phase": "patch_refresh_error", "step": "diff", "error": err.Error()})
		return
	}
	patchPath := filepath.Join(workDir, "patch.diff")
	// Derive observability stats so the harness can audit WHY a task
	// ended up with patch.diff missing even though update_file was
	// called — v6-strat showed sklearn-10297 with 2 update_file calls
	// but an empty patch, and without this meta event there was no
	// way to tell if git didn't see the change (gitignore, wrong path)
	// or the function itself was buggy.
	bytesOut := len(diffOut)
	fileCount := 0
	for _, line := range strings.Split(string(diffOut), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fileCount++
		}
	}
	// Only overwrite when the diff is non-empty OR the file is missing —
	// never clobber a correct patch.diff with an empty diff after a
	// failed revise turn.
	wrote := false
	if len(diffOut) > 0 {
		_ = os.WriteFile(patchPath, diffOut, 0o644)
		wrote = true
	}
	prevBytes := int64(0)
	if st, err := os.Stat(patchPath); err == nil {
		prevBytes = st.Size()
	}
	emitMeta(out, map[string]any{
		"phase":      "patch_refresh",
		"bytes":      bytesOut,
		"files":      fileCount,
		"wrote":      wrote,
		"patch_size": prevBytes,
	})
}

func execCmd(ctx context.Context, dir, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	return c.Run()
}

func execCmdOutput(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	return c.Output()
}

// hashBytes returns a cheap 64-bit fingerprint of a byte slice. Used by
// the critic revise loop to detect a "stuck" agent (patch unchanged
// between two consecutive rounds) without paying for a proper SHA.
func hashBytes(b []byte) uint64 {
	var h uint64 = 1469598103934665603 // FNV-1a 64 offset
	for _, x := range b {
		h ^= uint64(x)
		h *= 1099511628211
	}
	return h
}

// errString lets us drop error values straight into emitMeta maps. nil
// → empty string so the JSON payload stays compact when no error
// occurred; otherwise the error message.
func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// buildNudgePrompt is delivered as a continuation message when the first
// turn finished without producing patch.diff. The agent has already done
// its research in the same session — this prompt just forces it to stop
// investigating and commit to a best-guess patch. Grading rule: no patch
// = 0, wrong patch = partial credit via critic REVISE, so ANY patch beats
// silence.
func buildNudgePrompt() string {
	return "STOP. Your previous turn finished without producing patch.diff — that is an automatic 0.\n" +
		"You already have enough signal from the tools you ran. Do NOT call ast_search, read_file, " +
		"glob_search, or ripgrep_search again in this turn.\n\n" +
		"Now, in this order:\n" +
		"1. Pick the single most-suspect symbol / function based on what you've already read.\n" +
		"2. Call update_file on its source file with your best-guess minimal fix.\n" +
		"3. Regenerate patch.diff:\n" +
		"   cd repo && git add -A && git diff --cached > ../patch.diff && git restore --staged .\n\n" +
		"An imperfect patch will be reviewed by the critic and you will get a second chance to refine it. " +
		"No patch = no second chance. Commit now."
}

// buildRevisePrompt wraps the critic verdict so the agent can act on it.
// The agent already knows the original task — we just hand it the focused
// feedback plus a single instruction (re-fix and re-write patch.diff).
func buildRevisePrompt(criticFeedback string) string {
	return fmt.Sprintf(
		"Critic review of your previous patch:\n%s\n\n"+
			"Re-examine the relevant file(s) with read_file, apply a corrected fix via update_file, "+
			"then regenerate patch.diff:\n"+
			"  cd repo && git add -A && git diff --cached > ../patch.diff && git restore --staged .\n\n"+
			"Do NOT call the critic again. Submit the revised patch and stop.",
		criticFeedback,
	)
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
