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

	emitMeta(out, map[string]any{
		"phase":               "swe_bo_n_start",
		"n":                   n,
		"temp_min":            tempMin,
		"temp_max":            tempMax,
		"revise_per_phase":    revisePerPhase,
		"per_sample_timeout_s": int(effectiveTimeout.Seconds()),
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
		if !noPatchNudge && exitCode != 3 && !patchExistsAt(patchPath) {
			emitMeta(out, map[string]any{
				"phase":           "swe_bo_n_nudge",
				"index":           i,
				"after_exit_code": exitCode,
				"session_id":      sessionID,
			})
			_, _, _ = runOneTurn(out, pc, protocol.ChatRequest{
				SessionID:  fmt.Sprintf("%s-bo%d", sessionID, i),
				Message:    buildNudgePrompt(),
				WorkDir:    workDir,
				PromptType: protocol.PromptType(samplePromptType),
			}, nudgeTimeout, idleAbort, thinkingAbort, autoApprove)
			refreshPatchDiff(workDir, out)
			if patchExistsAt(patchPath) {
				c.NudgeFired = true
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
//	patch       = +1   (vs no patch)
//
// pytest_ran && !pytest_pass scores 0 (neutral) — a sample that tried
// shouldn't be ranked below one that didn't even reach pytest.
func scoreBoNCandidate(c bonCandidate) int {
	score := 0
	if c.HasPatch {
		score += 1
	}
	if c.PytestRan && c.PytestPass {
		score += 10
	}
	if c.CriticRan && c.CriticOK {
		score += 5
	}
	return score
}

// resetSWEWorkspace returns the SWE-bench task workspace to a clean
// pre-attempt state. Best-effort: missing repo/.git is silently
// tolerated so a workspace that lost its git state doesn't abort BO@N.
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
	for _, args := range [][]string{
		{"reset", "--hard", "HEAD"},
		{"clean", "-fd"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		_ = cmd.Run()
	}
}

func patchExistsAt(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return st.Size() > 0
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
