package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/critic"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// SWE-bench Best-of-N (BO@N) sampling with banya's verifier features.
//
// Strategy (b+) per the design discussion:
//
//   for i in 0..N-1:
//     A. workspace reset + temperature spread (diversity)
//     B. agent run               → patch_i
//     C. nudge if !patchExists   (1-shot)
//     D. critic review           (+ 1 revise round on reject)
//     E. pytest gate             (+ 1 revise round on fail)
//     F. score_i = pytest*10 + critic*5 + has_patch*1
//     G. backup patch.diff → patch.diff.bo<i>
//   winner = argmax(score)
//   patch.diff = winner backup
//
// Why this shape:
//
//   - Diversity comes from temperature spread (0.7→1.0); revise polishing
//     doesn't change the underlying strategy of a sample, only refines it.
//   - Per-sample revise is capped at 1 round per phase to keep total
//     compute around (a)+25% (vs full 3-round critic-revise that would 6x
//     the cost without obviously beating BO16's diversity payoff).
//   - Hybrid verifier mirrors the R2E paper's combination of execution
//     (pytest) and execution-free (LLM judge) signals.
//
// Activated by `--swe-bo-n N` (N > 1). When active, banya's standard
// post-agent nudge + critic-revise + pytest-gate-revise paths run *inside*
// each sample's per-iteration budget instead of once at the end.

type bonCandidate struct {
	Index       int
	Temperature float64
	PatchPath   string
	PatchHash   string
	HasPatch    bool

	NudgeFired      bool
	CommitForced    bool
	CriticRan       bool
	CriticOK        bool
	CriticRevised   bool
	PytestRan       bool
	PytestPass      bool
	PytestRevised   bool

	Score int
	Notes string
}

// runBoN drives `n` independent agent samples and writes the highest-
// scoring patch back to `patchPath`. Returns the winner candidate and
// the full list (already sorted by score desc).
func runBoN(
	ctx context.Context,
	out *bufio.Writer,
	pc *client.ProcessClient,
	sessionID string,
	promptText, promptTypeStr string,
	workDir, patchPath string,
	timeout, idleAbort, thinkingAbort, nudgeTimeout time.Duration,
	perSampleTimeout time.Duration, // 0 = auto (timeout / N)
	autoApprove bool,
	n int,
	tempMin, tempMax float64,
	noPatchNudge bool,
	reviewer *critic.Reviewer,
	criticEnabled bool,
	criticIssueFile string,
	pytestGateEnabled bool,
	pytestTimeoutS int,
	revisePerPhase int, // 1 in b+, 0 in pure (b)
) (winner *bonCandidate, candidates []bonCandidate) {

	if n < 2 {
		return nil, nil
	}

	// Per-sample timeout: when non-zero, each sample's runOneTurn (and its
	// critic-revise / pytest-revise turns) get THIS budget instead of the
	// overall task timeout. Without this, sample 0 can monopolise the whole
	// task budget, leaving sample 1 with effectively no time — observed in
	// the v6 smoke (astropy: sample 0 ~43 min, sample 1 4 tool calls).
	//
	// Auto mode (perSampleTimeout == 0): split overall timeout evenly across
	// N samples, with a 60s floor and a per-sample headroom for the b+
	// auxiliary turns (nudge / 2× revise) — those use their own timeouts so
	// the agent run itself can take effectively the full slice.
	effectiveTimeout := perSampleTimeout
	if effectiveTimeout <= 0 {
		effectiveTimeout = timeout / time.Duration(n)
		if effectiveTimeout < 60*time.Second {
			effectiveTimeout = 60 * time.Second
		}
	}

	// Wipe stale .agent state from prior runs of this workspace. SWE-bench
	// workspaces persist across launches (results/workspaces/<task_id>/),
	// so a previously-killed run leaves a `trajectory.jsonl` (and any
	// `trajectory.bo*.jsonl` backups) that can confuse the SIBDD
	// classifier and produce false BO@N events when the new run starts.
	// We tolerate missing dir / files silently — every workspace begins
	// fresh in BO@N's eyes.
	cleanStaleAgentDir(workDir, out)

	// Extract issue symbols once for this task — every sample's nudge
	// reuses the same list, since the issue is the same across samples.
	issueSymbols := extractIssueSymbols(promptText)

	emitMeta(out, map[string]any{
		"phase":               "swe_bo_n_start",
		"n":                   n,
		"temp_min":            tempMin,
		"temp_max":            tempMax,
		"revise_per_phase":    revisePerPhase,
		"per_sample_timeout_s": int(effectiveTimeout.Seconds()),
		"issue_symbols":       issueSymbols,
		"session_id":          sessionID,
	})

	candidates = make([]bonCandidate, 0, n)

	var issueText string
	if reviewer != nil {
		issueText, _ = readIssueForCritic(criticIssueFile, promptText)
	}

	for i := 0; i < n; i++ {
		// A. Reset workspace.
		resetSWEWorkspace(workDir, out)

		// Temperature spread for diversity.
		temp := tempMin
		if n > 1 {
			temp = tempMin + (tempMax-tempMin)*float64(i)/float64(n-1)
		}
		_ = os.Setenv("BANYA_SWE_BO_TEMPERATURE", fmt.Sprintf("%.3f", temp))
		topP := 0.9 + 0.05*float64(i)/float64(maxInt(1, n-1))
		_ = os.Setenv("BANYA_SWE_BO_TOP_P", fmt.Sprintf("%.3f", topP))

		emitMeta(out, map[string]any{
			"phase":       "swe_bo_n_sample_start",
			"index":       i,
			"temperature": temp,
			"top_p":       topP,
			"session_id":  sessionID,
		})

		c := bonCandidate{Index: i, Temperature: temp}

		// B. Agent run.
		samplePromptType := promptTypeStr
		_, exitReason, exitCode := runOneTurn(out, pc, protocol.ChatRequest{
			SessionID:  fmt.Sprintf("%s-bo%d", sessionID, i),
			Message:    promptText,
			WorkDir:    workDir,
			PromptType: protocol.PromptType(samplePromptType),
		}, effectiveTimeout, idleAbort, thinkingAbort, autoApprove)
		refreshPatchDiff(workDir, out)
		c.Notes = fmt.Sprintf("agent_exit=%s", exitReason)
		if exitCode != 0 {
			c.Notes += " (sample non-zero exit)"
		}

		// C. Nudge if no patch.diff (banya feature).
		//
		// Gate on `exitCode != 3` (sidecar / chat-start error) instead of
		// `exitCode == 0`. The original guard skipped the nudge whenever the
		// sample hit timeout (exitCode 2) — exactly the case where the
		// commit-now nudge has the highest leverage. Sidecar errors (3) are
		// still excluded because the chat session itself is unhealthy.
		// Extract the most-read source file from this sample's
		// trajectory so far — used to inject a concrete patch target
		// into both nudge and commit-force prompts (v11). Empty
		// string when the agent did no read_file calls; both prompts
		// fall back to symbol-only guidance in that case.
		fileHint := extractMostReadSourceFile(workDir)

		// v18 — Diff Linter: also re-fire nudge when the patch exists
		// but only touches reproducer/test/workspace files (the
		// "Reproducer-only Artefact" terminal-attractor we saw in
		// matplotlib v16 + django v16). Without this, a 0-byte source
		// diff that happens to include `repro.py` passes the original
		// gate and locks in score=0.
		patchOnlySandbox := patchHasOnlySandboxFiles(patchPath)
		if !noPatchNudge && exitCode != 3 && (!patchExistsAt(patchPath) || patchOnlySandbox) {
			emitMeta(out, map[string]any{
				"phase":              "swe_bo_n_nudge",
				"index":              i,
				"after_exit_code":    exitCode,
				"issue_symbols":      issueSymbols,
				"file_hint":          fileHint,
				"session_id":         sessionID,
				"reason":             ternStr(patchOnlySandbox, "reproducer_only", "empty"),
				"patch_only_sandbox": patchOnlySandbox,
			})
			_, _, _ = runOneTurn(out, pc, protocol.ChatRequest{
				SessionID:  fmt.Sprintf("%s-bo%d", sessionID, i),
				Message:    buildNudgePromptWithSymbols(issueSymbols, fileHint, patchOnlySandbox),
				WorkDir:    workDir,
				PromptType: protocol.PromptType(samplePromptType),
			}, nudgeTimeout, idleAbort, thinkingAbort, autoApprove)
			refreshPatchDiff(workDir, out)
			if patchExistsAt(patchPath) && !patchHasOnlySandboxFiles(patchPath) {
				c.NudgeFired = true
			}
		}

		// C2. Commit-force (paper §10.5.7 commit scaffold) — when even
		// the nudge produced no patch, run one more turn with a
		// trigger-word + first-person + concrete-file prompt that
		// primes the model to commit instead of explore (v11). Short
		// timeout (half of nudgeTimeout, capped at 600s) so we don't
		// burn budget on a non-committing model.
		//
		// v10 evidence (astropy-12907): even with the directive
		// "Your next tool call MUST be update_file", the EKTO model
		// defaulted to list_files / run_command. v11 swaps directive
		// for first-person trigger-word priming + concrete file path.
		// v18 — same Diff Linter gate as nudge phase.
		patchOnlySandboxCF := patchHasOnlySandboxFiles(patchPath)
		if !noPatchNudge && exitCode != 3 && (!patchExistsAt(patchPath) || patchOnlySandboxCF) {
			cfTimeout := nudgeTimeout / 2
			if cfTimeout > 600*time.Second {
				cfTimeout = 600 * time.Second
			}
			if cfTimeout < 60*time.Second {
				cfTimeout = 60 * time.Second
			}
			// Re-extract fileHint — the nudge phase may have added
			// fresh reads that change the most-read target.
			fileHint = extractMostReadSourceFile(workDir)
			emitMeta(out, map[string]any{
				"phase":              "swe_bo_n_commit_force",
				"index":              i,
				"issue_symbols":      issueSymbols,
				"file_hint":          fileHint,
				"timeout_s":          int(cfTimeout.Seconds()),
				"session_id":         sessionID,
				"reason":             ternStr(patchOnlySandboxCF, "reproducer_only", "empty"),
				"patch_only_sandbox": patchOnlySandboxCF,
			})
			_, _, _ = runOneTurn(out, pc, protocol.ChatRequest{
				SessionID:  fmt.Sprintf("%s-bo%d", sessionID, i),
				Message:    buildCommitForcePrompt(issueSymbols, fileHint, patchOnlySandboxCF),
				WorkDir:    workDir,
				PromptType: protocol.PromptType(samplePromptType),
			}, cfTimeout, idleAbort, thinkingAbort, autoApprove)
			refreshPatchDiff(workDir, out)
			if patchExistsAt(patchPath) && !patchHasOnlySandboxFiles(patchPath) {
				c.CommitForced = true
			}
		}

		// D. Critic review (+ 1 revise on reject if budget > 0).
		if criticEnabled && reviewer != nil && patchExistsAt(patchPath) {
			data, _ := os.ReadFile(patchPath)
			rCtx, rCancel := context.WithTimeout(ctx, 90*time.Second)
			decision, cerr := reviewer.ReviewPatch(rCtx, issueText, string(data), workDir)
			rCancel()
			if cerr == nil {
				c.CriticRan = true
				c.CriticOK = decision.OK
				if !decision.OK && revisePerPhase > 0 {
					revisePrompt := buildRevisePrompt(decision.Feedback)
					_, _, _ = runOneTurn(out, pc, protocol.ChatRequest{
						SessionID:  fmt.Sprintf("%s-bo%d", sessionID, i),
						Message:    revisePrompt,
						WorkDir:    workDir,
						PromptType: protocol.PromptType(samplePromptType),
					}, effectiveTimeout, idleAbort, thinkingAbort, autoApprove)
					refreshPatchDiff(workDir, out)
					c.CriticRevised = true
					if patchExistsAt(patchPath) {
						data2, _ := os.ReadFile(patchPath)
						r2Ctx, r2Cancel := context.WithTimeout(ctx, 90*time.Second)
						decision2, cerr2 := reviewer.ReviewPatch(r2Ctx, issueText, string(data2), workDir)
						r2Cancel()
						if cerr2 == nil {
							c.CriticOK = decision2.OK
						}
					}
				}
			}
		}

		// E. Pytest gate (+ 1 revise on fail if budget > 0).
		if pytestGateEnabled && patchExistsAt(patchPath) {
			patched := extractPatchedFiles(patchPath)
			testFiles := findTestFilesForModule(workDir, patched)
			if len(testFiles) > 0 {
				pCtx, pCancel := context.WithTimeout(ctx, time.Duration(pytestTimeoutS)*time.Second)
				pass, feedback := runProjectPytest(pCtx, workDir, testFiles, pytestTimeoutS)
				pCancel()
				c.PytestRan = true
				c.PytestPass = pass
				if !pass && revisePerPhase > 0 {
					revisePrompt := buildPytestRevisePrompt(testFiles, feedback)
					_, _, _ = runOneTurn(out, pc, protocol.ChatRequest{
						SessionID:  fmt.Sprintf("%s-bo%d", sessionID, i),
						Message:    revisePrompt,
						WorkDir:    workDir,
						PromptType: protocol.PromptType(samplePromptType),
					}, effectiveTimeout, idleAbort, thinkingAbort, autoApprove)
					refreshPatchDiff(workDir, out)
					c.PytestRevised = true
					if patchExistsAt(patchPath) {
						p2Ctx, p2Cancel := context.WithTimeout(ctx, time.Duration(pytestTimeoutS)*time.Second)
						pass2, _ := runProjectPytest(p2Ctx, workDir, testFiles, pytestTimeoutS)
						p2Cancel()
						c.PytestPass = pass2
					}
				}
			}
		}

		// F. Final state of patch.diff (after all phases).
		if patchExistsAt(patchPath) {
			c.HasPatch = true
			data, _ := os.ReadFile(patchPath)
			h := sha1.Sum(data)
			c.PatchHash = hex.EncodeToString(h[:])[:12]
		}
		c.Score = scoreBoNCandidate(c)

		// G. Backup patch.diff for later restoration of the winner.
		if c.HasPatch {
			backup := fmt.Sprintf("%s.bo%d", patchPath, i)
			if err := os.Rename(patchPath, backup); err == nil {
				c.PatchPath = backup
			}
		}

		emitMeta(out, map[string]any{
			"phase":          "swe_bo_n_sample_done",
			"index":          i,
			"has_patch":      c.HasPatch,
			"patch_hash":     c.PatchHash,
			"nudge_fired":    c.NudgeFired,
			"commit_forced":  c.CommitForced,
			"critic_ran":     c.CriticRan,
			"critic_ok":      c.CriticOK,
			"critic_revised": c.CriticRevised,
			"pytest_ran":     c.PytestRan,
			"pytest_pass":    c.PytestPass,
			"pytest_revised": c.PytestRevised,
			"score":          c.Score,
			"session_id":     sessionID,
		})
		candidates = append(candidates, c)

		// Trajectory backup. The next sample's banya-core invocation
		// truncates `.agent/trajectory.jsonl` (observed empirically in v6:
		// sample 0 had ~41 tool calls but its trajectory was wiped when
		// sample 1 started). When BANYA_SWE_BO_KEEP_LOSERS=1 is set, copy
		// the full trajectory to `trajectory.bo<i>.jsonl` so the SIBDD
		// exporter can materialise per-sample preference data.
		if os.Getenv("BANYA_SWE_BO_KEEP_LOSERS") == "1" {
			src := filepath.Join(workDir, ".agent", "trajectory.jsonl")
			dst := filepath.Join(workDir, ".agent", fmt.Sprintf("trajectory.bo%d.jsonl", i))
			if err := copyFileForBackup(src, dst); err == nil {
				emitMeta(out, map[string]any{
					"phase":      "swe_bo_n_trajectory_backup",
					"index":      i,
					"path":       dst,
					"session_id": sessionID,
				})
			}
		}

		_ = os.Unsetenv("BANYA_SWE_BO_TEMPERATURE")
		_ = os.Unsetenv("BANYA_SWE_BO_TOP_P")
	}

	// Pick winner.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.PytestPass != b.PytestPass {
			return a.PytestPass
		}
		return a.Index < b.Index
	})

	if len(candidates) > 0 && candidates[0].HasPatch {
		w := candidates[0]
		_ = os.Rename(w.PatchPath, patchPath)
		winner = &w
	}

	emitMeta(out, map[string]any{
		"phase":         "swe_bo_n_done",
		"n":             n,
		"winner_index":  optInt(winner, -1),
		"winner_score":  optScore(winner),
		"winner_pytest": optPytest(winner),
		"winner_critic": optCritic(winner),
		"session_id":    sessionID,
	})

	// Loser-patch cleanup. SIBDD's eval-as-rollout exporter (paper
	// pipeline §10.5.7) needs the loser patches to materialise (chosen,
	// rejected) preference pairs from a single BO@N run. Setting
	// `BANYA_SWE_BO_KEEP_LOSERS=1` leaves `patch.diff.bo<i>` files in the
	// workspace so the exporter can read them; otherwise we clean up so
	// the workspace footprint stays small.
	keepLosers := os.Getenv("BANYA_SWE_BO_KEEP_LOSERS") == "1"
	for _, c := range candidates {
		if winner != nil && c.Index == winner.Index {
			continue
		}
		if c.PatchPath == "" {
			continue
		}
		if keepLosers {
			continue
		}
		_ = os.Remove(c.PatchPath)
	}

	return winner, candidates
}

// scoreBoNCandidate combines hybrid verifier signals into one integer.
//
//	pytest pass = +10  (strongest, mirrors hidden test suite)
//	critic OK   = +5   (LLM judge agreement)
//	patch       = +1   (vs no patch) — but ONLY if at least one verifier
//	                   accepts (v18.1: closes the perverse-incentive loophole
//	                   where a patch rejected by both verifiers still wins
//	                   over no-patch via has_patch=1).
//
// v18.1 changes (paper §9.8 evidence — Hallucination Conservation):
//
//   - **Verifier-rejected patches no longer beat no-patch.**
//     v18 evidence (django + matplotlib): both samples produced patches
//     where critic_ok=false AND pytest_pass=false (or pytest skipped),
//     yet has_patch=1 made them winners. The model learned to submit
//     "confident garbage" instead of failing honestly. Now: if both
//     verifiers reject, has_patch contributes 0 — tying with no-patch
//     so the tiebreaker prefers the cleaner no-patch sample (which can
//     re-trigger nudge in a fresh turn).
//   - **pytest_skipped is treated as not-passing (unchanged from before
//     but explicitly documented).** The condition `c.PytestRan && c.PytestPass`
//     already required pytest to actually run; skipped pytest still scores
//     0. v18 matplotlib won via has_patch=1 because the model fabricated
//     a NEW module that pytest couldn't find tests for → pytest_skipped
//     → pytest_score=0 → has_patch=1 carried the win. The fix in (1)
//     above closes this hole because critic_ok was also false.
//
// Behavioural matrix (post v18.1):
//
//	critic_ok  pytest_pass  has_patch  →  score
//	  true       true         true     →  16   (both verifiers accept)
//	  true       false/skip   true     →   6   (critic-only accept)
//	  false      true         true     →  11   (pytest-only accept; rare)
//	  false      false/skip   true     →   0   (rejected — was 1 pre-v18.1)
//	  -          -            false    →   0
func scoreBoNCandidate(c bonCandidate) int {
	score := 0
	pytestAccepted := c.PytestRan && c.PytestPass
	criticAccepted := c.CriticRan && c.CriticOK
	if c.HasPatch && (pytestAccepted || criticAccepted) {
		score += 1
	}
	if pytestAccepted {
		score += 10
	}
	if criticAccepted {
		score += 5
	}
	return score
}

// resetSWEWorkspace returns the SWE-bench task workspace to a clean
// pre-attempt state. Best-effort: missing repo/.git is silently
// tolerated so a workspace that lost its git state doesn't abort BO@N.
//
// v18.1 — three changes addressing workspace leak found in matplotlib v18:
//
//  1. `git clean -fdx` (was `-fd`): also remove .gitignore'd files. Some
//     repos (e.g., matplotlib) generate files like `_version.py` from
//     setuptools_scm that are gitignored but still pollute later samples
//     if left from a previous attempt.
//
//  2. Capture stderr from each git command and log it on non-zero exit.
//     Previously errors were silently swallowed (`_ = cmd.Run()`), so a
//     failed reset between samples was indistinguishable from a successful
//     one. Sample 0/1 cross-contamination was undetectable.
//
//  3. Verify post-reset cleanliness with `git status --porcelain`. If
//     untracked files remain after `clean -fdx`, log them as a warning so
//     future debugging has a signal.
func resetSWEWorkspace(workDir string, out *bufio.Writer) {
	repoRoot := filepath.Join(workDir, "repo")

	for _, name := range []string{
		"patch.diff", "repro.py", "reproduce_issue.py",
	} {
		_ = os.Remove(filepath.Join(workDir, name))
	}

	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil {
		emitMeta(out, map[string]any{
			"phase":  "swe_bo_n_reset_skipped",
			"reason": "no .git in repo/",
		})
		return
	}
	resetIssues := []string{}
	for _, args := range [][]string{
		{"reset", "--hard", "HEAD"},
		{"clean", "-fdx"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		out2, err := cmd.CombinedOutput()
		if err != nil {
			resetIssues = append(resetIssues,
				fmt.Sprintf("git %s: %s (output: %s)",
					strings.Join(args, " "), err.Error(), strings.TrimSpace(string(out2))))
		}
	}
	// Verify cleanliness — if anything is still untracked/modified after
	// the reset, the next sample will start in a polluted workspace.
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = repoRoot
	if statusOut, err := statusCmd.Output(); err == nil {
		dirty := strings.TrimSpace(string(statusOut))
		if dirty != "" {
			resetIssues = append(resetIssues,
				fmt.Sprintf("workspace still dirty after reset: %s", dirty))
		}
	}
	if len(resetIssues) > 0 {
		emitMeta(out, map[string]any{
			"phase":  "swe_bo_n_reset_warning",
			"issues": resetIssues,
		})
	}
}

func patchExistsAt(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return st.Size() > 0
}

// patchHasOnlySandboxFiles returns true when patch.diff exists and is
// non-empty BUT every file it touches is a reproducer / test / workspace
// scratch file rather than a real source-tree edit.
//
// v18 (paper §9.7 follow-up — "Reproducer-only Artefact" terminal-attractor
// finding from v16 matplotlib + django): the forced-commit phase + create-
// reproducer step in the scaffold form a behavioural sink. Whatever
// upstream failure happens (Localization / Mutation / Edit-Avoidance), the
// agent's "I should commit something" subroutine fires → it creates
// repro.py + runs git diff → patch.diff captures only repro.py. The hidden
// test grader applies the patch and gets … a new repro.py at workspace
// root, source untouched, hidden test still fails (score 0).
//
// The Diff Linter treats reproducer-only patches the same as empty
// patches for nudge / commit-force gating, so the agent gets one more
// chance to write a real source edit instead of accepting the sink.
//
// Sandbox-file heuristic:
//   - basenames `repro.py`, `reproduce_issue.py`, `patch.diff`, `plan.md`
//   - basenames matching `repro_*.py`
//   - test paths under `/tests/` or `/test/`, or `test_*.py` / `*_test.py`
//   - any path NOT under `repo/` (workspace scratch files)
//
// Returns false when the patch touches at least one real source file under
// `repo/` (other than the heuristics above) — that's a real edit, even if
// it's wrong, and the regular flow handles it.
func patchHasOnlySandboxFiles(patchPath string) bool {
	if !patchExistsAt(patchPath) {
		return false
	}
	files := extractPatchedFiles(patchPath)
	if len(files) == 0 {
		return false
	}
	for _, p := range files {
		if !isSandboxPath(p) {
			return false
		}
	}
	return true
}

func isSandboxPath(p string) bool {
	// Strip any leading "a/" or "b/" defensively.
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	base := filepath.Base(p)
	switch base {
	case "repro.py", "reproduce_issue.py", "patch.diff", "plan.md":
		return true
	}
	if strings.HasPrefix(base, "repro_") && strings.HasSuffix(base, ".py") {
		return true
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	if strings.HasSuffix(base, "_test.py") {
		return true
	}
	if strings.Contains(p, "/tests/") || strings.Contains(p, "/test/") {
		return true
	}
	// Workspace-root scratch (not under repo/): treat as sandbox.
	if !strings.HasPrefix(p, "repo/") {
		return true
	}
	return false
}

func ternStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func optInt(c *bonCandidate, fallback int) int {
	if c == nil {
		return fallback
	}
	return c.Index
}

func optScore(c *bonCandidate) int {
	if c == nil {
		return -1
	}
	return c.Score
}

func optPytest(c *bonCandidate) string {
	if c == nil {
		return "unknown"
	}
	if !c.PytestRan {
		return "skipped"
	}
	if c.PytestPass {
		return "pass"
	}
	return "fail"
}

func optCritic(c *bonCandidate) string {
	if c == nil {
		return "unknown"
	}
	if !c.CriticRan {
		return "skipped"
	}
	if c.CriticOK {
		return "ok"
	}
	return "reject"
}

// cleanStaleAgentDir removes any prior-run trajectory.jsonl and
// trajectory.bo<i>.jsonl backups from the workspace's `.agent/` dir at
// the start of a fresh BO@N run. Patches and other workspace files are
// handled by `resetSWEWorkspace` per-sample; this helper specifically
// targets agent telemetry files that survived a killed previous run.
//
// v7 evidence: matplotlib's workspace `.agent/trajectory.jsonl` carried
// `swe_bo_n_sample_done` events from a v6 run that was killed mid-task,
// causing the SIBDD classifier to read those stale events as if they
// belonged to v7.
func cleanStaleAgentDir(workDir string, out *bufio.Writer) {
	agentDir := filepath.Join(workDir, ".agent")
	st, err := os.Stat(agentDir)
	if err != nil || !st.IsDir() {
		return
	}
	removed := 0
	if err := os.Remove(filepath.Join(agentDir, "trajectory.jsonl")); err == nil {
		removed++
	}
	matches, _ := filepath.Glob(filepath.Join(agentDir, "trajectory.bo*.jsonl"))
	for _, p := range matches {
		if err := os.Remove(p); err == nil {
			removed++
		}
	}
	if removed > 0 {
		emitMeta(out, map[string]any{
			"phase":          "swe_bo_n_clean_stale_agent",
			"removed_count":  removed,
			"agent_dir":      agentDir,
		})
	}
}

// copyFileForBackup copies src → dst for the BO@N per-sample trajectory
// backup (paper §10.5.7). Best-effort: a missing src is silently tolerated
// (some samples may not have produced a trajectory if the agent crashed
// at chat-start time).
func copyFileForBackup(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
