package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	cmd.Flags().Bool("swe-pytest-gate", false, "SWE-bench only: after critic OK, run the project's pytest file for the modified module. If it fails, treat as critic NOT-OK and trigger one revise round. Auto-enabled when BANYA_SWE_BENCH=1.")
	cmd.Flags().Int("swe-pytest-timeout", 240, "Per-pytest-invocation timeout in seconds (default 240).")
	cmd.Flags().Int("swe-bo-n", 1, "SWE-bench Best-of-N sampling: when N>1, run N independent agent samples per task (each with its own temperature) and pick the best by hybrid verifier (pytest gate + critic). Default 1 = off. Recommended N=4 for testing, N=16 for paper-comparable.")
	cmd.Flags().Float64("swe-bo-temp-min", 0.7, "BO@N: minimum temperature across samples (linear spread to --swe-bo-temp-max). Default 0.7.")
	cmd.Flags().Float64("swe-bo-temp-max", 1.0, "BO@N: maximum temperature across samples. Default 1.0.")
	cmd.Flags().Int("swe-bo-revise-rounds", 1, "BO@N per-sample revise budget: 0 = pure BO@N (no per-sample revise), 1 = b+ default (1 critic-revise + 1 pytest-revise per sample), 2+ = aggressive polishing per sample.")
	cmd.Flags().Duration("swe-bo-per-sample-timeout", 0, "BO@N per-sample timeout (default 0 = auto: overall --timeout / N, with 60s floor). Without this, sample 0 can monopolise the whole task budget and starve later samples — observed empirically.")
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
	swePytestGate, _ := cmd.Flags().GetBool("swe-pytest-gate")
	swePytestTimeout, _ := cmd.Flags().GetInt("swe-pytest-timeout")
	// Auto-enable when SWE-bench mode env is set, so the harness doesn't need
	// to add another flag to BANYA_CLI_EXTRA_ARGS.
	if !swePytestGate && os.Getenv("BANYA_SWE_BENCH") == "1" {
		swePytestGate = true
	}
	sweBoN, _ := cmd.Flags().GetInt("swe-bo-n")
	sweBoTempMin, _ := cmd.Flags().GetFloat64("swe-bo-temp-min")
	sweBoTempMax, _ := cmd.Flags().GetFloat64("swe-bo-temp-max")
	sweBoReviseRounds, _ := cmd.Flags().GetInt("swe-bo-revise-rounds")
	sweBoPerSampleTimeout, _ := cmd.Flags().GetDuration("swe-bo-per-sample-timeout")
	// Env override for harness convenience.
	if envN := os.Getenv("BANYA_SWE_BO_N"); envN != "" {
		if v, err := strconv.Atoi(envN); err == nil && v > sweBoN {
			sweBoN = v
		}
	}

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
	patchPath := filepath.Join(workDir, "patch.diff")

	// SWE-bench BO@N (Strategy b+): when --swe-bo-n>1 (or BANYA_SWE_BO_N>1)
	// AND we are not in a Web-Bench layout, replace the standard single-shot
	// agent + nudge + critic-revise + pytest-gate-revise chain with N
	// independent samples. runBoN() applies the same banya verifier features
	// per sample (capped revise budget) and writes the winner's patch back
	// to patch.diff. After it returns we skip directly to the exit event.
	wbLayoutEarly := webbench.Detect(workDir)
	if sweBoN > 1 && !wbLayoutEarly.Active() {
		var reviewer *critic.Reviewer
		if criticEnabled {
			reviewer = critic.NewFromEnv()
		}
		bonSessionSeed := fmt.Sprintf("bo-%d", time.Now().UnixNano())
		winner, _ := runBoN(
			cmd.Context(), out, pc,
			bonSessionSeed,
			promptText, promptTypeStr,
			workDir, patchPath,
			timeout, idleAbort, thinkingAbort, nudgeTimeout,
			sweBoPerSampleTimeout,
			autoApprove,
			sweBoN, sweBoTempMin, sweBoTempMax,
			noPatchNudge,
			reviewer, criticEnabled, criticIssueFile,
			swePytestGate, swePytestTimeout,
			sweBoReviseRounds,
		)
		exitReason := "swe_bo_n_done"
		exitCode := 0
		if winner == nil {
			exitReason = "swe_bo_n_no_winner"
			exitCode = 2
		}
		emitMeta(out, map[string]any{
			"phase":      "exit",
			"reason":     exitReason,
			"elapsed_ms": time.Since(start).Milliseconds(),
		})
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return nil
	}

	// First turn — the agent's initial attempt.
	sessionID, exitReason, exitCode := runOneTurn(out, pc, protocol.ChatRequest{
		Message:    promptText,
		WorkDir:    workDir,
		PromptType: protocol.PromptType(promptTypeStr),
	}, timeout, idleAbort, thinkingAbort, autoApprove)
	refreshPatchDiff(workDir, out)

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
	// v18 — Diff Linter: also re-fire when patch.diff exists but only
	// touches reproducer/test/workspace files (the matplotlib v16 sample 1
	// terminal-attractor pattern: "found right file, never edited it,
	// committed only repro.py").
	// v2 Parallel B child-mode signal tracking. When BANYA_SWE_BO_CHILD_INDEX
	// is set in env, this banya-cli is running as a BO@N child. We track
	// the verifier signals across phases here and emit a bo_meta.json at
	// exit so the parent process can read them and apply scoreBoNCandidate.
	// Outside child mode these vars are dead — no behavior change.
	childIndex, _ := strconv.Atoi(os.Getenv("BANYA_SWE_BO_CHILD_INDEX"))
	isBoNChild := os.Getenv("BANYA_SWE_BO_CHILD_INDEX") != ""
	var childCriticRan, childCriticOK, childCriticRevised bool
	var childPytestRan, childPytestPass, childPytestRevised bool
	childStartedAt := time.Now()

	// v18.6 fix #5 — incremental bo_meta.json snapshot helper. Called at
	// each phase boundary so that even if the child gets SIGKILL'd in a
	// later phase (parent timeout, OOM, panic), the parent's reader sees
	// the latest known state instead of treating has_patch=false. Critical
	// for SIBDD forensics: v18.5 astropy bo0 evidence — 117KB log, 42 actions
	// of in-progress work was thrown away because the child died before
	// reaching the final bo_meta write at exit.
	//
	// Each call overwrites bo_meta.json with the current snapshot of all
	// per-phase signals. Best-effort — failures are logged and don't abort.
	snapshotBoMeta := func() {
		if !isBoNChild {
			return
		}
		hasPatch := patchExists()
		var patchHash string
		if hasPatch {
			if data, err := os.ReadFile(patchPath); err == nil {
				h := sha1.Sum(data)
				patchHash = hex.EncodeToString(h[:])[:12]
			}
		}
		_ = writeBoChildMeta(out, workDir, boChildMeta{
			Index:         childIndex,
			HasPatch:      hasPatch,
			PatchHash:     patchHash,
			PatchPath:     patchPath,
			NudgeFired:    nudgedThisRun,
			CommitForced:  false,
			CriticRan:     childCriticRan,
			CriticOK:      childCriticOK,
			CriticRevised: childCriticRevised,
			PytestRan:     childPytestRan,
			PytestPass:    childPytestPass,
			PytestRevised: childPytestRevised,
			WallElapsedS:  int(time.Since(childStartedAt).Seconds()),
		})
	}

	reproducerOnlyAtTopNudge := patchHasOnlySandboxFiles(patchPath)
	if !wbLayout.Active() && !noPatchNudge && !nudgedThisRun && exitCode != 3 && (!patchExists() || reproducerOnlyAtTopNudge) {
		nudgedThisRun = true
		issueSymbols := extractIssueSymbols(promptText)
		fileHint := extractMostReadSourceFileValidated(workDir, issueSymbols)
		emitMeta(out, map[string]any{
			"phase":              "nudge",
			"prev_exit":          exitReason,
			"issue_symbols":      issueSymbols,
			"file_hint":          fileHint,
			"session_id":         sessionID,
			"reason":             ternStr(reproducerOnlyAtTopNudge, "reproducer_only", "empty"),
			"patch_only_sandbox": reproducerOnlyAtTopNudge,
		})
		_, exitReason, exitCode = runOneTurn(out, pc, protocol.ChatRequest{
			SessionID:  sessionID, // continue the same conversation
			Message:    buildNudgePromptWithSymbols(issueSymbols, fileHint, reproducerOnlyAtTopNudge),
			WorkDir:    workDir,
			PromptType: protocol.PromptType(promptTypeStr),
		}, nudgeTimeout, idleAbort, thinkingAbort, autoApprove)
		refreshPatchDiff(workDir, out)
		snapshotBoMeta() // v18.6 fix #5 — checkpoint after nudge phase
	}

	// v18.6 — second-chance Diff Linter. The original nudge fires once.
	// v18.5 evidence (matplotlib): after the first nudge, the model
	// produced a patch.diff containing ONLY fabricated test files
	// (final_test.py, issue_example.py, test_comprehensive.py) — all
	// sandbox per isSandboxPath, but Diff Linter never re-checked
	// because nudgedThisRun was already true. Submitted patch was
	// effectively a reproducer-only artefact.
	//
	// Second-chance fires when ALL of:
	//   - main nudge already fired (nudgedThisRun==true, so the model
	//     had a chance and squandered it on tests)
	//   - patch exists AND every file is sandbox classification
	//   - exit isn't a chat-start error (sidecar still alive)
	// Builds a stronger commit-force-style prompt that explicitly
	// names "no more test files; modify the source file under repo/".
	if !wbLayout.Active() && !noPatchNudge && nudgedThisRun && exitCode != 3 &&
		patchExists() && patchHasOnlySandboxFiles(patchPath) {
		issueSymbols := extractIssueSymbols(promptText)
		fileHint := extractMostReadSourceFileValidated(workDir, issueSymbols)
		emitMeta(out, map[string]any{
			"phase":              "nudge_second_chance",
			"reason":             "post_nudge_sandbox_only",
			"issue_symbols":      issueSymbols,
			"file_hint":          fileHint,
			"session_id":         sessionID,
			"patch_only_sandbox": true,
		})
		// Use buildCommitForcePrompt with reproducerOnly=true to get
		// the "LAST CHANCE — your patch.diff only modifies reproducer
		// / test / workspace files" framing that's stronger than the
		// regular nudge prompt.
		_, exitReason, exitCode = runOneTurn(out, pc, protocol.ChatRequest{
			SessionID:  sessionID,
			Message:    buildCommitForcePrompt(issueSymbols, fileHint, true),
			WorkDir:    workDir,
			PromptType: protocol.PromptType(promptTypeStr),
		}, nudgeTimeout/2, idleAbort, thinkingAbort, autoApprove)
		refreshPatchDiff(workDir, out)
		snapshotBoMeta() // v18.6 fix #5 — checkpoint after second-chance Diff Linter
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
				childCriticRan = true
				if cerr == nil {
					childCriticOK = decision.OK
				}
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
				childCriticRevised = true
				snapshotBoMeta() // v18.6 fix #5 — checkpoint after critic-revise
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

	// SWE-bench post-critic pytest gate (authoritative).
	//
	// Runs AFTER the critic loop ends — regardless of how it ended (OK,
	// reject, budget_exhausted, revise_stuck). The hidden test suite IS
	// the project's pytest file, so passing pytest is the strongest
	// alignment signal available. If pytest passes, accept the patch
	// regardless of critic verdict; if it fails, do one final revise turn
	// with the pytest output as feedback, then re-check pytest.
	//
	// Decision matrix:
	//   pytest PASS                → ACCEPT (overrides critic reject/stuck)
	//   pytest FAIL → revise → PASS → ACCEPT
	//   pytest FAIL → revise → FAIL → fall through (critic verdict stands)
	//   no test files / disabled    → no-op (critic decision stands)
	if swePytestGate && !wbLayout.Active() && exitCode == 0 && patchExists() {
		patchedFiles := extractPatchedFiles(patchPath)
		pytestTestFiles := findTestFilesForModule(workDir, patchedFiles)
		if len(pytestTestFiles) > 0 {
			pCtx, pCancel := context.WithTimeout(cmd.Context(), time.Duration(swePytestTimeout)*time.Second)
			pytestPassed, pytestFeedback := runProjectPytest(pCtx, workDir, pytestTestFiles, swePytestTimeout)
			pCancel()
			childPytestRan = true
			childPytestPass = pytestPassed
			emitMeta(out, map[string]any{
				"phase":          "swe_pytest_gate",
				"stage":          "post_critic",
				"passed":         pytestPassed,
				"test_files":     pytestTestFiles,
				"feedback_chars": len(pytestFeedback),
				"session_id":     sessionID,
			})
			if !pytestPassed {
				// One final revise turn with pytest feedback.
				revisePrompt := buildPytestRevisePrompt(pytestTestFiles, pytestFeedback)
				_, exitReason, exitCode = runOneTurn(out, pc, protocol.ChatRequest{
					SessionID:  sessionID,
					Message:    revisePrompt,
					WorkDir:    workDir,
					PromptType: protocol.PromptType(promptTypeStr),
				}, timeout, idleAbort, thinkingAbort, autoApprove)
				refreshPatchDiff(workDir, out)
				childPytestRevised = true
				snapshotBoMeta() // v18.6 fix #5 — checkpoint after pytest-revise
				if exitCode == 0 && patchExists() {
					pCtx2, pCancel2 := context.WithTimeout(cmd.Context(), time.Duration(swePytestTimeout)*time.Second)
					pytestPassed2, _ := runProjectPytest(pCtx2, workDir, pytestTestFiles, swePytestTimeout)
					pCancel2()
					childPytestPass = pytestPassed2
					emitMeta(out, map[string]any{
						"phase":      "swe_pytest_gate",
						"stage":      "post_revise",
						"passed":     pytestPassed2,
						"session_id": sessionID,
					})
				}
			}
		} else {
			emitMeta(out, map[string]any{
				"phase":         "swe_pytest_gate_skipped",
				"reason":        "no test file found for patched modules",
				"patched_files": patchedFiles,
				"session_id":    sessionID,
			})
		}
		snapshotBoMeta() // v18.6 fix #5 — checkpoint after pytest gate
	}

	// v2 Parallel B — child mode: emit bo_meta.json so the parent can
	// reconstruct a bonCandidate and apply scoreBoNCandidate. We compute
	// has_patch + patch_hash here from the canonical patchPath.
	if isBoNChild {
		hasPatch := patchExists()
		var patchHash string
		if hasPatch {
			if data, err := os.ReadFile(patchPath); err == nil {
				h := sha1.Sum(data)
				patchHash = hex.EncodeToString(h[:])[:12]
			}
		}
		_ = writeBoChildMeta(out, workDir, boChildMeta{
			Index:         childIndex,
			HasPatch:      hasPatch,
			PatchHash:     patchHash,
			PatchPath:     patchPath,
			NudgeFired:    nudgedThisRun,
			CommitForced:  false, // commit-force only exists in BO@N path; child runs regular flow
			CriticRan:     childCriticRan,
			CriticOK:      childCriticOK,
			CriticRevised: childCriticRevised,
			PytestRan:     childPytestRan,
			PytestPass:    childPytestPass,
			PytestRevised: childPytestRevised,
			WallElapsedS:  int(time.Since(childStartedAt).Seconds()),
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

	// Write-tool deadline (BANYA_SWE_WRITE_TOOL_DEADLINE_S) — abort the
	// turn if no patch-producing tool (update_file / create_file /
	// remove_file / run_command) has been called within this many seconds.
	// Catches the v6 matplotlib pattern: 24 list_files / read_file calls
	// with zero update_file before timeout.
	var writeDeadlineCh <-chan time.Time
	var writeDeadlineTimer *time.Timer
	if v := os.Getenv("BANYA_SWE_WRITE_TOOL_DEADLINE_S"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			writeDeadlineTimer = time.NewTimer(time.Duration(secs) * time.Second)
			writeDeadlineCh = writeDeadlineTimer.C
		}
	}
	sawWriteTool := false

	// Repeated-identical-call detection (BANYA_SWE_REPEAT_CALL_LIMIT) —
	// when the same (toolName, input) is observed N consecutive times,
	// abort. Catches v6 matplotlib's `list_files .` ×3 and similar
	// shotgunning patterns where the agent ignores prior observations.
	repeatLimit := 0
	if v := os.Getenv("BANYA_SWE_REPEAT_CALL_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			repeatLimit = n
		}
	}
	var lastCallSig string
	repeatCount := 0
	repeatedCallSig := ""

	// Plan-stage tracking (paper §10.5.7) — every tool call is mapped
	// to one of seven plan stages and a `plan_stage_enter` meta event
	// is emitted on transition. Useful for post-hoc SIBDD analysis:
	// "samples that hit FIX = succeeded; samples stuck in STUDY =
	// capability ceiling". Counts also feed the future v12
	// stage-stuck nudge.
	currentStage := stageUnknown
	stageCounts := map[planStage]int{}
	sawFix := false

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
				// Write-tool deadline: stop the timer permanently the
				// first time we see a write tool (the agent is now
				// committing to changes, no need to keep watching).
				toolName := extractStringField(evt.Data, "tool_name")
				if !sawWriteTool && isWriteTool(toolName) {
					sawWriteTool = true
					if writeDeadlineTimer != nil {
						writeDeadlineTimer.Stop()
						writeDeadlineCh = nil
					}
				}
				// Repeated-identical-call detection.
				if repeatLimit > 0 {
					sig := buildToolCallSignature(toolName, evt.Data)
					if sig != "" && sig == lastCallSig {
						repeatCount++
						if repeatCount >= repeatLimit {
							repeatedCallSig = sig
							exitReason = "repeated_identical_call"
							exitCode = 2
							break loop
						}
					} else {
						lastCallSig = sig
						repeatCount = 1
					}
				}
				// Plan-stage classification.
				inputPath, inputCmd := extractToolInputs(evt.Data)
				if !sawFix && (toolName == "update_file" || toolName == "create_file" || toolName == "remove_file") {
					sawFix = true
				}
				stage := classifyStage(toolName, inputPath, inputCmd, sawFix)
				if stage != stageUnknown && stage != currentStage {
					emitMeta(out, map[string]any{
						"phase":      "plan_stage_enter",
						"from_stage": currentStage.String(),
						"to_stage":   stage.String(),
						"tool":       toolName,
					})
					currentStage = stage
				}
				stageCounts[stage]++
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
		case <-writeDeadlineCh:
			// No write tool (update_file / create_file / remove_file /
			// run_command) was called within the deadline. The agent is
			// almost certainly stuck in a read/search exploration loop;
			// terminate so the b+ nudge can fire with the
			// commit-now instruction.
			exitReason = "write_tool_deadline"
			exitCode = 2
			break loop
		}
	}
	if exitReason == "repeated_identical_call" {
		emitMeta(out, map[string]any{
			"phase":     "repeated_call_abort",
			"signature": repeatedCallSig,
			"count":     repeatCount,
			"limit":     repeatLimit,
		})
	}
	if exitReason == "write_tool_deadline" {
		emitMeta(out, map[string]any{
			"phase": "write_tool_deadline_abort",
		})
	}
	// Plan-stage summary at turn-end so SIBDD can read a single
	// per-turn stage histogram without re-walking the trajectory.
	if len(stageCounts) > 0 {
		summary := map[string]int{}
		for k, v := range stageCounts {
			summary[k.String()] = v
		}
		emitMeta(out, map[string]any{
			"phase":         "plan_stage_summary",
			"final_stage":   currentStage.String(),
			"stage_counts":  summary,
			"saw_fix":       sawFix,
			"total_calls":   func() int { n := 0; for _, v := range stageCounts { n += v }; return n }(),
		})
	}
	return sessionID, exitReason, exitCode
}

// extractToolInputs pulls the typical SWE-bench tool input fields
// (path / command) out of an EventToolCallStart payload. Returns
// empty strings when the field is missing or the payload is mis-
// shaped — callers must tolerate that.
func extractToolInputs(data any) (path, command string) {
	m, ok := data.(map[string]any)
	if !ok {
		return "", ""
	}
	input, ok := m["input"].(map[string]any)
	if !ok {
		return "", ""
	}
	if s, ok := input["path"].(string); ok {
		path = s
	}
	if s, ok := input["command"].(string); ok {
		command = s
	}
	return path, command
}

// isWriteTool returns true for tools that produce a code-mutating
// effect on the source tree. The write-tool deadline is satisfied as
// soon as any of these is observed.
//
// `run_command` is intentionally EXCLUDED here — empirically (v8
// django, action 12: `cat patch.diff`) the agent uses run_command for
// read-only diagnostics that should not satisfy the deadline. Real
// patch creation requires staged edits, which only the file-mutation
// tools produce. Removing run_command tightens the safety-net to its
// intended purpose: catch the "24 read/searches without ever editing"
// pattern.
func isWriteTool(name string) bool {
	switch name {
	case "update_file", "create_file", "remove_file":
		return true
	}
	return false
}

// buildToolCallSignature renders a stable string for the
// repeated-identical-call detector. Uses the tool name plus a JSON
// re-marshalling of the input map so identical args always hash the
// same. Returns "" when the tool name is missing — that disables the
// detector for the malformed event rather than producing false
// positives.
func buildToolCallSignature(toolName string, data any) string {
	if toolName == "" {
		return ""
	}
	var input any
	if m, ok := data.(map[string]any); ok {
		input = m["input"]
	} else {
		// Fall back to JSON round-trip — same idiom as extractStringField.
		b, err := json.Marshal(data)
		if err != nil {
			return toolName + ":?"
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			input = m["input"]
		}
	}
	if input == nil {
		return toolName + ":∅"
	}
	b, err := json.Marshal(input)
	if err != nil {
		return toolName + ":?"
	}
	return toolName + ":" + string(b)
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
// extractIssueSymbols pulls candidate symbol names from the SWE-bench
// task prompt so they can be injected into the nudge prompt. The
// extraction is heuristic — backtick-wrapped tokens that look like
// Python identifiers — and we cap the result at 8 most-frequent
// symbols to avoid drowning the nudge in irrelevant noise.
//
// Evolution of this function:
//   v9: extracted from the WHOLE prompt — picked up banya tool names
//       (`update_file`, `ast_search`, …) and the git base-commit hash.
//       Polluted nudges with framework jargon.
//   v10: added blocklist for tool names + git-hash regex.
//   v12: noticed the v9-style failure resurface on django-10914 because
//       the scaffold preamble itself names example symbols (`_cstack`,
//       `Table.to_pandas`) which then leak into every task's nudge. The
//       fix is to slice the prompt at the `### Issue` marker (added by
//       agent-evaluation/src/banya_eval/benchmarks/swe_bench.py) and
//       extract symbols ONLY from the text after that marker — i.e. the
//       actual issue body, not the framework boilerplate.
//
// Fallback: if the marker is missing (e.g. when this is called from a
// non-SWE-bench codepath), we extract from the whole prompt with the
// expanded blocklist as a safety net.
var nonIssueSymbolBlocklist = map[string]bool{
	// Banya/R2E tool names — these appear in the scaffold prompt and
	// SWE-bench plan boilerplate, not in the actual issue body.
	"update_file": true, "create_file": true, "remove_file": true,
	"run_command": true, "read_file": true, "ripgrep_search": true,
	"glob_search": true, "ast_search": true, "list_files": true,
	"run_reproducer": true, "finish_task": true, "load_skill": true,
	"spawn_agent": true, "ExitPlanMode": true, "WebFetch": true,
	// Workspace structural names.
	"patch.diff": true, "repro.py": true, "reproduce_issue.py": true,
	"plan.md": true, "testbed": true,
	// Python keywords / language noise.
	"True": true, "False": true, "None": true, "self": true, "cls": true,
	"True/False": true,
	// Shell / verbs that occasionally appear in backticks.
	"cd": true, "ls": true, "git": true, "diff": true, "stage": true,
	// Scaffold preamble noise — concrete symbols named in the
	// SWE-specific protocol section as illustrative examples
	// (`_cstack`, `Table.to_pandas`) or in meta-discussion text
	// (`unresolved` — used to describe failure modes, not as a code
	// symbol). v11 django-10914 evidence: these contaminated the nudge
	// when extraction ran on the whole prompt. The marker-based slice
	// (above) is the primary defence; this list is the safety net for
	// the fallback path.
	"_cstack": true, "Table.to_pandas": true, "unresolved": true,
	// v18.6 — Platform / OS / hosting names that surface in SWE-bench
	// issue bodies (often as bug-environment context: "I'm on CentOS",
	// "see this Stack Overflow thread"). The bare-identifier regex
	// fallback (v18.5) catches these as CamelCase candidates, but
	// they're never the symbol the patch should target. v18.5 django
	// evidence: issueSymbols included "CentOS" and "GitHub" alongside
	// the actual bug symbols (FILE_UPLOAD_PERMISSION, FileSystemStorage),
	// polluting the nudge prompt's symbol list.
	"CentOS": true, "Ubuntu": true, "Debian": true, "RedHat": true,
	"Fedora": true, "Arch": true, "Linux": true, "Windows": true,
	"MacOS": true, "macOS": true, "Darwin": true, "BSD": true,
	"FreeBSD": true, "OpenBSD": true, "Alpine": true, "NixOS": true,
	"GitHub": true, "GitLab": true, "Bitbucket": true, "SourceForge": true,
	"StackOverflow": true, "Stack": true, "Overflow": true,
	"Reddit": true, "Twitter": true, "Discord": true, "Slack": true,
	"Docker": true, "Kubernetes": true, "VSCode": true, "PyCharm": true,
	"IntelliJ": true, "Sublime": true, "Vim": true, "Emacs": true,
	// Common version control / CI identifiers in prose.
	"CI": true, "CD": true, "PR": true, "MR": true, "API": true,
	"URL": true, "URI": true, "HTTP": true, "HTTPS": true,
	// Issue-tracker conventions.
	"TODO": true, "FIXME": true, "XXX": true, "NOTE": true,
}

var gitHashRe = regexp.MustCompile(`^[a-f0-9]{7,40}$`)

// issueBodyMarkerRe matches the `### Issue` heading the SWE-bench
// harness inserts to delimit the start of the verbatim issue body.
var issueBodyMarkerRe = regexp.MustCompile(`(?m)^\s*###\s+Issue\s*$`)

// sliceToIssueBody returns the substring of `prompt` starting from the
// first `### Issue` heading (exclusive). Returns "" when the marker is
// absent — caller should fall back to the whole prompt with the
// blocklist applied.
func sliceToIssueBody(prompt string) string {
	loc := issueBodyMarkerRe.FindStringIndex(prompt)
	if loc == nil {
		return ""
	}
	return prompt[loc[1]:]
}

func extractIssueSymbols(prompt string) []string {
	if prompt == "" {
		return nil
	}
	// Primary path: extract from issue body only. The scaffold preamble
	// (which names example symbols like `_cstack`) is excluded by the
	// slice — eliminates the entire class of cross-task contamination.
	target := sliceToIssueBody(prompt)
	if target == "" {
		// Fallback: marker missing; use the whole prompt and rely on
		// the blocklist + git-hash filter to catch obvious noise.
		target = prompt
	}
	// Primary pass: match `<identifier>` (backtick-quoted), optionally with
	// a trailing `()` to capture method invocations. This is the high-
	// precision path — if the issue body explicitly emphasises a symbol
	// in backticks, prefer those.
	reBacktick := regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]{1,80})(\\(\\))?`")
	matches := reBacktick.FindAllStringSubmatch(target, -1)

	counts := map[string]int{}
	for _, m := range matches {
		name := m[1]
		if len(name) < 3 {
			continue
		}
		if nonIssueSymbolBlocklist[name] {
			continue
		}
		if gitHashRe.MatchString(name) {
			continue
		}
		counts[name]++
	}

	// v18.5 — Fallback for plain-text issues. v18.4 evidence:
	// django ("Set default FILE_UPLOAD_PERMISSION to 0o644") and flask
	// ("Raise error when blueprint name contains a dot") use plain-text
	// identifiers without backticks. The backtick-only regex returned
	// nil, leaving nudge/commit-force prompts without symbol context.
	// When the primary pass yielded zero hits, scan for "bug-shaped"
	// identifiers — names that have at least one of: an underscore, an
	// ALL-CAPS prefix of length >= 3, or CamelCase (capital + lowercase
	// + capital). This filter excludes English prose words like "would",
	// "default", "permissions" while catching FILE_UPLOAD_PERMISSION,
	// FileSystemStorage, Blueprint, etc.
	if len(counts) == 0 {
		reBare := regexp.MustCompile("[A-Za-z_][A-Za-z0-9_]{2,80}")
		// Pre-compiled identifier "shape" tests.
		hasUnderscore := func(s string) bool { return strings.ContainsAny(s, "_") }
		isAllCapsPrefix := func(s string) bool {
			if len(s) < 3 {
				return false
			}
			caps := 0
			for _, r := range s {
				if r >= 'A' && r <= 'Z' {
					caps++
				} else {
					break
				}
			}
			return caps >= 3
		}
		isCamelCase := func(s string) bool {
			if len(s) < 4 || s[0] < 'A' || s[0] > 'Z' {
				return false
			}
			hasLower := false
			hasInnerCap := false
			for i := 1; i < len(s); i++ {
				c := s[i]
				if c >= 'a' && c <= 'z' {
					hasLower = true
				} else if c >= 'A' && c <= 'Z' && hasLower {
					hasInnerCap = true
				}
			}
			return hasInnerCap
		}
		for _, name := range reBare.FindAllString(target, -1) {
			if len(name) < 4 {
				continue
			}
			if !hasUnderscore(name) && !isAllCapsPrefix(name) && !isCamelCase(name) {
				continue
			}
			if nonIssueSymbolBlocklist[name] {
				continue
			}
			if gitHashRe.MatchString(name) {
				continue
			}
			counts[name]++
		}
	}

	if len(counts) == 0 {
		return nil
	}
	type kv struct {
		k string
		v int
	}
	ranked := make([]kv, 0, len(counts))
	for k, v := range counts {
		ranked = append(ranked, kv{k, v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].v != ranked[j].v {
			return ranked[i].v > ranked[j].v
		}
		return ranked[i].k < ranked[j].k
	})
	if len(ranked) > 8 {
		ranked = ranked[:8]
	}
	out := make([]string, 0, len(ranked))
	for _, kv := range ranked {
		out = append(out, kv.k)
	}
	return out
}

func buildNudgePrompt() string {
	return buildNudgePromptWithSymbols(nil, "", false)
}

// buildNudgePromptWithSymbols renders the commit-now nudge with an
// optional concrete list of issue symbols extracted from the prompt
// AND an optional file-hint (the most-frequently-read source file in
// the trajectory so far). When both are present, the agent has nearly
// zero ambiguity about WHERE to apply the fix.
//
// `reproducerOnly` (v18) — true when patch.diff exists but only adds
// repro.py / test files / workspace scratch (the v16 matplotlib + django
// terminal-attractor pattern). Switches the lead diagnosis from "patch is
// empty" to "patch only adds reproducer/tests" and tightens guidance to
// stop the agent from re-committing the same useless artefact.
func buildNudgePromptWithSymbols(symbols []string, fileHint string, reproducerOnly bool) string {
	symbolHint := ""
	if len(symbols) > 0 {
		quoted := make([]string, len(symbols))
		for i, s := range symbols {
			quoted[i] = "`" + s + "`"
		}
		symbolHint = "\nThe issue text names these symbols (most-frequent first): " +
			strings.Join(quoted, ", ") + ".\n" +
			"Your patch MUST modify a file containing one of these symbols. " +
			"Use ripgrep_search to confirm the symbol's location if you don't already know it.\n"
	}
	if fileHint != "" {
		symbolHint += "Most-read file so far: `" + fileHint + "` — that is almost certainly your patch target.\n"
	}

	if reproducerOnly {
		// v18 — Diff Linter path. The agent already has *a* patch.diff,
		// but it only modifies reproducer / test / workspace files. The
		// hidden test grader will apply this patch and find ZERO source
		// changes → automatic 0. Tell the agent exactly that.
		return "STOP. Your `patch.diff` exists but only modifies reproducer / test / workspace " +
			"scratch files (e.g., `repro.py`, `reproduce_issue.py`, `test_*.py`). It does NOT touch " +
			"any real source file under `repo/`. The hidden-test grader applies this patch and finds " +
			"zero source changes → automatic 0.\n" +
			symbolHint +
			"\n" +
			"You may have read the right file already. Reading is not enough — you must EDIT it.\n" +
			"\n" +
			"You already have enough signal. Do NOT call ast_search, read_file, glob_search, or " +
			"ripgrep_search again in this turn. Do NOT re-create or re-run the reproducer — the " +
			"reproducer is already in `patch.diff` and that's the problem.\n\n" +
			"Now, in this order:\n" +
			"1. Pick the symbol *explicitly named in the issue text* (function, class, method, " +
			"attribute). Do NOT pick a different symbol you happen to know about in this repo.\n" +
			"2. Call **update_file** (SEARCH/REPLACE) or **replace_lines** (line-number replacement, " +
			"better for constants/settings files) on the source file containing that symbol with your " +
			"best-guess minimal fix. This step is mandatory — skipping it leaves you with the same " +
			"reproducer-only patch you have now.\n" +
			"3. Regenerate patch.diff (only AFTER step 2):\n" +
			"   cd repo && git add -A && git diff --cached > ../patch.diff && git restore --staged .\n" +
			"4. Verify the new patch.diff modifies a file under `repo/` (not just `repro.py`):\n" +
			"   grep '^diff --git' patch.diff\n" +
			"   You should see a line referencing your source file under `repo/...`. If you only see " +
			"`repro.py`, you skipped step 2 — go back and call update_file/replace_lines.\n" +
			"\n" +
			"An imperfect source patch will be reviewed by the critic and you will get a second " +
			"chance to refine it. A reproducer-only patch is rejected and locks in score=0. Commit a " +
			"real source edit now."
	}

	return "STOP. Your previous turn finished without producing patch.diff (or with patch.diff EMPTY — " +
		"0 bytes) — that is an automatic 0.\n" +
		symbolHint +
		"\n" +
		"Diagnosis hint: an empty patch.diff usually means you ran `git diff --cached` BEFORE staging " +
		"any code edits. Calling git diff without first calling `update_file` produces an empty diff. " +
		"You MUST call update_file (or create_file) at least once on a real source file BEFORE " +
		"regenerating the patch.\n" +
		"\n" +
		"You already have enough signal from the tools you ran. Do NOT call ast_search, read_file, " +
		"glob_search, or ripgrep_search again in this turn.\n\n" +
		"Now, in this order:\n" +
		"1. Pick the symbol *explicitly named in the issue text* (function, class, method, attribute). " +
		"Do NOT pick a different symbol you happen to know about in this repo — that is hallucination, " +
		"and a patch on the wrong symbol scores 0 even if it looks reasonable. The hidden tests only " +
		"exercise the named symbol.\n" +
		"2. Call update_file on the source file *that contains that symbol* with your best-guess " +
		"minimal fix. (skipping this step is the most common cause of empty patch.diff — do not skip)\n" +
		"3. Regenerate patch.diff (only AFTER the update_file in step 2):\n" +
		"   cd repo && git add -A && git diff --cached > ../patch.diff && git restore --staged .\n" +
		"4. Verify patch.diff is non-empty: `ls -l patch.diff`. If it shows 0 bytes, you skipped " +
		"step 2 — go back and call update_file.\n" +
		"\n" +
		"An imperfect patch will be reviewed by the critic and you will get a second chance to refine it. " +
		"No patch = no second chance. Commit now."
}

// extractMostReadSourceFile scans the workspace's `.agent/trajectory.jsonl`
// and returns the most-frequently-read source path under `repo/`. Test
// files are deliberately skipped — the issue is in the source tree, not
// the test suite.
//
// Used by the nudge / commit-force prompts to inject a CONCRETE file
// path the model has already invested attention in. v10 evidence:
// astropy sample 1 read `repo/astropy/modeling/separable.py` 5+ times
// without ever calling update_file. Saying "edit a file containing one
// of these symbols" left the choice to the model; saying "edit
// `repo/astropy/modeling/separable.py` (you've read this 5 times)"
// removes the choice. The file the model already invested in is
// almost certainly the right one.
//
// Returns "" when the trajectory has no read_file actions yet.
//
// Wraps extractMostReadSourceFileValidated with no symbol cross-check,
// preserved for non-SWE callers that don't have issueSymbols.
func extractMostReadSourceFile(workDir string) string {
	return extractMostReadSourceFileValidated(workDir, nil)
}

// extractMostReadSourceFileValidated is v18.2's anti-amplifier version
// of fileHint extraction. When issueSymbols is non-empty, the returned
// path is validated to actually contain at least one of those symbols
// (cheap grep). This prevents the v18 flask-4045 failure mode where
// the model's misdirected exploration (reading json/tag.py 3+ times
// despite the issue being about Blueprint validation) crystallised
// into a commit-force prompt that hardcoded the wrong file.
//
// Behaviour matrix:
//
//	issueSymbols == nil/empty       → return most-read (legacy behaviour)
//	issueSymbols set, validated hit → return validated most-read file
//	issueSymbols set, no hit at all → return "" (signals "no usable hint")
//
// The empty-string fallback is the key correctness move: when the
// fileHint cannot be validated, the caller (commit-force / nudge)
// falls back to "edit a file containing one of these symbols" guidance
// without hardcoding a wrong file. v10 astropy success path is
// preserved (separable.py contains _cstack — validation passes).
// v18 flask failure path is severed (json/tag.py does not contain
// Blueprint — validation fails, hint suppressed).
func extractMostReadSourceFileValidated(workDir string, issueSymbols []string) string {
	trajPath := filepath.Join(workDir, ".agent", "trajectory.jsonl")
	f, err := os.Open(trajPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	counts := map[string]int{}
	dec := json.NewDecoder(f)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		var ev struct {
			Kind     string `json:"kind"`
			ToolName string `json:"toolName"`
			Input    struct {
				Path string `json:"path"`
			} `json:"input"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		if ev.Kind != "action" || ev.ToolName != "read_file" {
			continue
		}
		path := ev.Input.Path
		if path == "" || !strings.HasPrefix(path, "repo/") {
			continue
		}
		if strings.Contains(path, "/tests/") || strings.Contains(path, "/test/") {
			continue
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") {
			continue
		}
		counts[path]++
	}
	if len(counts) == 0 {
		return ""
	}

	// Sort candidates by count desc, path asc (for determinism).
	type cand struct {
		path string
		n    int
	}
	cands := make([]cand, 0, len(counts))
	for p, n := range counts {
		cands = append(cands, cand{p, n})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].n != cands[j].n {
			return cands[i].n > cands[j].n
		}
		return cands[i].path < cands[j].path
	})

	// No symbol filter → legacy behaviour: return the top-1.
	if len(issueSymbols) == 0 {
		return cands[0].path
	}

	// v18.2 — validate top candidates against issueSymbols. We check up
	// to top-5 to allow some slack for the model's distraction; if none
	// of the top-5 most-read files contain any issue symbol, the hint
	// would be misleading and we suppress it.
	maxCheck := 5
	if len(cands) < maxCheck {
		maxCheck = len(cands)
	}
	for i := 0; i < maxCheck; i++ {
		absPath := cands[i].path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(workDir, absPath)
		}
		// v18.6 — prefer files that DEFINE a symbol over files that
		// merely reference one. v18.5 evidence (astropy v18.4 fileHint
		// = `core.py` because `from .separable import separability_matrix`
		// is a substring match): substring validation passes import
		// lines too, locking the model onto the wrong file.
		if fileDefinesAnySymbol(absPath, issueSymbols) {
			return cands[i].path
		}
	}
	// No top-N read file contains an issue symbol — suppress the hint.
	return ""
}

// fileContainsAnySymbol returns true when `path` is readable and
// contains at least one of the symbols as a substring. Cheap byte
// match. Kept for backwards compat / non-Python files.
func fileContainsAnySymbol(path string, symbols []string) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() > 2*1024*1024 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	body := string(data)
	for _, s := range symbols {
		if s != "" && strings.Contains(body, s) {
			return true
		}
	}
	return false
}

// fileDefinesAnySymbol is v18.6's stricter validator. Returns true
// only if the file contains a *definition* (class def, function def,
// or top-level assignment) for any of the symbols. Substring matches
// inside import lines, comments, docstrings, or other-file references
// do NOT count.
//
// v18.5 astropy evidence: `core.py` matched `separability_matrix` via
// the `from .separable import separability_matrix` import line. The
// model patched core.py instead of separable.py (where the symbol is
// actually defined). Definition-level matching catches this.
//
// Patterns matched (Python multi-line, ^ anchored to line start):
//   - `class <NAME>` / `class <NAME>:` / `class <NAME>(` (class def)
//   - `def <NAME>(` (function/method def)
//   - `<NAME> = ` at column 0 (top-level assignment / constant)
//
// Returns false on read errors / very large files (>2MB) — falls back
// to the legacy substring path in that case via the caller.
func fileDefinesAnySymbol(path string, symbols []string) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() > 2*1024*1024 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	body := string(data)
	for _, s := range symbols {
		if s == "" {
			continue
		}
		quoted := regexp.QuoteMeta(s)
		// class def: `class NAME` followed by `:`, `(`, or space
		// (?m): multiline, ^ matches start of any line
		patClass := regexp.MustCompile(`(?m)^\s*class\s+` + quoted + `\b`)
		patFunc := regexp.MustCompile(`(?m)^\s*(async\s+)?def\s+` + quoted + `\s*\(`)
		// Top-level assignment: NAME at column 0 (no leading whitespace)
		// followed by optional type annotation and `=`. Catches:
		//   FILE_UPLOAD_PERMISSIONS = None
		//   version_info: tuple = (3, 7, 1)
		patAssign := regexp.MustCompile(`(?m)^` + quoted + `\s*(:[^=]+)?\s*=`)
		if patClass.MatchString(body) || patFunc.MatchString(body) || patAssign.MatchString(body) {
			return true
		}
	}
	return false
}

// classifyStage maps a single tool invocation to one of the seven
// SWE-bench plan stages defined in the r2egym scaffold. Used by
// runOneTurn to emit `plan_stage_enter` meta events so the SIBDD
// classifier can build per-task stage profiles for the paper's
// failure-mode analysis (§10.5.7 — eval-as-rollout supervision).
//
// Stages:
//   1 ORIENT     — read plan.md, locate the workspace
//   2 LOCATE     — find the target symbol (ast/glob/ripgrep)
//   3 STUDY      — read source files in narrow ranges
//   4 FIX        — update_file / create_file / remove_file
//   5 REPRODUCE  — run_reproducer / run a reproducer script
//   6 EDGE_CASES — additional reads after a fix (cross-checking)
//   7 SAVE       — git diff --cached → patch.diff
type planStage int

const (
	stageUnknown   planStage = 0
	stageOrient    planStage = 1
	stageLocate    planStage = 2
	stageStudy     planStage = 3
	stageFix       planStage = 4
	stageReproduce planStage = 5
	stageEdgeCases planStage = 6
	stageSave      planStage = 7
)

func (s planStage) String() string {
	switch s {
	case stageOrient:
		return "orient"
	case stageLocate:
		return "locate"
	case stageStudy:
		return "study"
	case stageFix:
		return "fix"
	case stageReproduce:
		return "reproduce"
	case stageEdgeCases:
		return "edge_cases"
	case stageSave:
		return "save"
	}
	return "unknown"
}

func classifyStage(toolName, path, command string, sawFix bool) planStage {
	switch toolName {
	case "update_file", "create_file", "remove_file":
		return stageFix
	case "run_reproducer":
		return stageReproduce
	case "ast_search", "ripgrep_search", "glob_search":
		if sawFix {
			return stageEdgeCases
		}
		return stageLocate
	case "list_files":
		if sawFix {
			return stageEdgeCases
		}
		return stageLocate
	case "read_file":
		if strings.HasSuffix(path, "plan.md") {
			return stageOrient
		}
		if sawFix {
			return stageEdgeCases
		}
		return stageStudy
	case "run_command":
		if strings.Contains(command, "git diff") || strings.Contains(command, "git add") {
			return stageSave
		}
		if sawFix {
			return stageReproduce
		}
		return stageStudy
	}
	return stageUnknown
}

// buildCommitForcePrompt is the maximally-directive last-resort message
// sent in BO@N's commit-force phase (paper §10.5.7) when both the agent
// run and the nudge failed to produce a non-empty patch.diff.
//
// v11 changes: replaces the directive ("MUST be update_file") with
// trigger-word + first-person priming. v10 evidence: even with the
// directive prompt, the EKTO model defaulted to list_files / run_command
// because the directive language pushed it into rule-following mode
// rather than action mode. v11 substitutes:
//   - First-person commitment seed ("I have enough context. I'll edit X")
//   - Concrete file target (extractMostReadSourceFile result)
//   - update_file format example (primes the call shape)
//   - Strong commit verbs ("patch", "modify") matching R2E rollout
//     trajectory style the EKTO model was trained on
//
// `fileHint` is the most-frequently-read source file in the agent's
// trajectory so far — empty string when extraction yielded nothing,
// in which case we fall back to symbol-only guidance.
func buildCommitForcePrompt(symbols []string, fileHint string, reproducerOnly bool) string {
	var b strings.Builder
	if reproducerOnly {
		b.WriteString("LAST CHANCE — your `patch.diff` only modifies reproducer / test / workspace " +
			"files. It does NOT touch any real source file under `repo/`. The hidden grader will reject " +
			"this and you'll score 0. This turn is for committing a real source edit, not for further " +
			"exploration or for re-running the reproducer.\n\n")
	} else {
		b.WriteString("LAST CHANCE — no patch.diff yet (or it's 0 bytes). The conversation has gathered enough context; this turn is for committing the fix, not for further exploration.\n\n")
	}

	if len(symbols) > 0 {
		quoted := make([]string, len(symbols))
		for i, s := range symbols {
			quoted[i] = "`" + s + "`"
		}
		b.WriteString("Issue symbols (your patch must touch a file containing one of these): " +
			strings.Join(quoted, ", ") + "\n")
	}
	if fileHint != "" {
		b.WriteString("Most-read source file in this conversation: `" + fileHint + "`. " +
			"That is your patch target unless you have a stronger reason to pick a different file.\n")
	}
	b.WriteString("\n")

	// First-person priming: seed the model's response so it completes
	// the pattern (commit) instead of restarting (explore).
	if fileHint != "" {
		b.WriteString("Begin your next response with this exact sentence (then continue normally):\n")
		b.WriteString("    \"I have enough context. I'll patch `" + fileHint + "` now to fix the issue.\"\n\n")
	} else {
		b.WriteString("Begin your next response with: \"I have enough context. I'll apply the fix now.\"\n\n")
	}

	// Format example primes the tool call shape. Keep this short —
	// v13 evidence (astropy 14K mega-patch with 7 files) showed that
	// adding lots of warnings to commit-force backfires: the model
	// reads the verbose prompt as an invitation to elaborate rather
	// than as a directive to commit. v14 trims back to the v12 shape
	// (single 1-3 line edit hint inline in the format example, no
	// separate EDIT GRANULARITY subsection).
	b.WriteString("Then call update_file with a SHORT SEARCH (1-3 lines, copied verbatim from the file — not a whole method):\n")
	b.WriteString("    update_file(\n")
	if fileHint != "" {
		b.WriteString("      path=\"" + fileHint + "\",\n")
	} else {
		b.WriteString("      path=\"repo/<package>/<module>.py\",\n")
	}
	b.WriteString("      old_string=\"<the 1-3 buggy lines, verbatim>\",\n")
	b.WriteString("      new_string=\"<the fixed line(s)>\"\n")
	b.WriteString("    )\n\n")

	b.WriteString("Then call run_command exactly once to save the patch:\n")
	b.WriteString("    run_command(\"cd repo && git add -A && git diff --cached > ../patch.diff && git restore --staged .\")\n\n")

	// Forbidden tools — the model needs an explicit avoid-list.
	b.WriteString("Forbidden this turn: `ast_search`, `read_file`, `ripgrep_search`, `glob_search`, `list_files`. You already have all the context you need; calling them again is the failure mode that brought us here.\n\n")

	// Decision rule + safety net.
	b.WriteString("If the exact edit is uncertain: make the smallest plausible patch that touches the symbol named in the issue. Common minimal fixes that often work:\n")
	b.WriteString("  - add the missing recursion case to the function named in the issue\n")
	b.WriteString("  - add the missing branch / guard for the input shape the issue describes\n")
	b.WriteString("  - flip the boolean operator on the most-suspect comparison\n\n")
	b.WriteString("A wrong patch is graded by critic + pytest and can earn partial credit. NO patch = automatic 0. The verifier decides correctness; this turn is for committing.")
	return b.String()
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
