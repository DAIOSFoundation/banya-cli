package main

// SWE-bench runtime enforcement patches.
//
// Background: prompt-level "patches 1/2/3" (banya-core default.ts edits)
// were empirically falsified on the v15-v16 weak-repo pool (v17 paired
// re-run: 7/20 baseline -> 4/20 treatment, -15pp regression). The
// directional reading is that Qwen3.5-397B's long-prompt attention
// budget is already saturated — adding rules at the bottom of an 80-line
// system prompt is "suggestion", not "constraint". The model silently
// ignored the new structure and the extra tokens displaced rules that
// previously worked.
//
// This file re-implements the three patches as POST-TURN runtime
// enforcement in banya-cli:
//
//   Patch 1 — Cause-locus enforcement. After the first runOneTurn we
//   parse .agent/trajectory.jsonl. If update_file fired without a
//   preceding "<thought>Symptom site:/Cause site:" block, we `git
//   checkout .` to revert the patch and fire a forced re-think nudge
//   that demands the analysis BEFORE the next update_file.
//
//   Patch 2 — Forced revert on thrash. If update_file count >= 3 and
//   patch.diff exceeds 60 lines (gold-patch p90), we hard-revert and
//   fire a "your approach was wrong, find a SMALLER fix at a
//   DIFFERENT location" nudge.
//
//   Patch 3 — Symmetry pre-scan. Before the first turn, regex-scan the
//   issue text for "should also be added" / "analog of" / "behaves the
//   same way" / similar phrasings. If matched, prepend a structurally
//   inserted ALERT block to the user prompt. This is the only one of
//   the three that's pre-turn, but it's still runtime: the model
//   receives the alert in conversation, not as one more rule among 30
//   in the system prompt.
//
// All three patches are gated on BANYA_SWE_RUNTIME_PATCHES=1 so the
// default behaviour is unchanged for non-SWE callers and so we can do
// paired A/B runs against the v16 baseline.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// runtimePatchesEnabled returns true when the post-turn runtime
// enforcement should fire. Default off — paper §5.2.1's paired-run
// methodology requires us to toggle this cleanly.
func runtimePatchesEnabled() bool {
	return os.Getenv("BANYA_SWE_RUNTIME_PATCHES") == "1"
}

// -------------------------------------------------------------------
// Patch 3 — Symmetry pre-scan (issue-text regex; pre-turn injection)
// -------------------------------------------------------------------

// symmetryPatterns matches issue-text phrasings that imply a SYMMETRIC
// fix is required (the named site + an analog). Captures the v15/v16
// mode C failure (flask-4045: "An error was already added for endpoint
// names in 1.0, but should have been added for this as well" — gold
// patch fixed BOTH sites).
var symmetryPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bshould also (be added|raise|support|behave|return|check|reject|validate|happen|apply|fail|warn)`),
	regexp.MustCompile(`(?i)\bwas (already|previously) (added|done|fixed|implemented)\b[^.]{1,200}\bbut\b`),
	regexp.MustCompile(`(?i)\b(analog|analogous|equivalent|counterpart)\s+(of|to|for)\b`),
	regexp.MustCompile(`(?i)\bbehaves (the same way|similarly|identically)\b`),
	regexp.MustCompile(`(?i)\bsame (behaviour|behavior|treatment|handling) as\b`),
	regexp.MustCompile(`(?i)\bmirror(s|ed)?\s+(the|that)\b`),
}

// detectSymmetryAlert scans a prompt/issue text and returns a non-empty
// ALERT block + the matched phrase if a symmetric-fix pattern is
// present.
func detectSymmetryAlert(text string) (alert string, matched string) {
	for _, re := range symmetryPatterns {
		if m := re.FindString(text); m != "" {
			block := "\n\n==== SYMMETRY ALERT (runtime pre-scan) ====\n" +
				"This issue contains the phrase: \"" + m + "\"\n" +
				"Symmetric-fix pattern detected. Gold-quality patches for issues with this\n" +
				"shape typically modify TWO sites:\n" +
				"  (a) the NEW site named in the issue, AND\n" +
				"  (b) the EXISTING site that the issue references as the analog — verify its\n" +
				"      current shape matches what the new site needs (e.g. `assert` should\n" +
				"      become `raise ValueError` so both sites raise the same exception type).\n" +
				"Before saving patch.diff: search the codebase for the analog (try grep/ripgrep\n" +
				"on the existing keyword) and confirm it has been brought into matching shape.\n" +
				"A patch fixing ONLY the new site WILL fail hidden tests that probe the analog.\n" +
				"==========================================\n"
			return block, m
		}
	}
	return "", ""
}

// -------------------------------------------------------------------
// Shared — trajectory.jsonl post-turn analysis
// -------------------------------------------------------------------

type turnAnalysis struct {
	UpdateFileCount        int
	CauseLocusBeforeWrite  bool // true if <thought> w/ Cause site:+Symptom site: was emitted before first update_file
	HasReproducer          bool
	LastReproducerErrored  bool
}

var (
	causeLocusRe   = regexp.MustCompile(`(?i)cause\s+(site|locus)\s*:`)
	symptomLocusRe = regexp.MustCompile(`(?i)symptom\s+site\s*:`)
)

// analyzeTrajectory walks <workDir>/.agent/trajectory.jsonl (written by
// the banya-eval harness) and surfaces the signals Patches 1 + 2 need.
//
// Returns a zero-value turnAnalysis on any read/parse error — Patch
// triggers must defensive-default to "do nothing" so a broken
// trajectory never causes spurious reverts.
func analyzeTrajectory(workDir string) turnAnalysis {
	out := turnAnalysis{}
	trajPath := filepath.Join(workDir, ".agent", "trajectory.jsonl")
	f, err := os.Open(trajPath)
	if err != nil {
		return out
	}
	defer f.Close()

	sawFirstUpdate := false
	var pending strings.Builder // accumulated assistant content up to but not including the current action

	scanner := bufio.NewScanner(f)
	// Trajectory lines can be large (full tool inputs / file contents).
	scanner.Buffer(make([]byte, 1024*1024), 32*1024*1024)

	for scanner.Scan() {
		var d map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			continue
		}
		kind, _ := d["kind"].(string)
		switch kind {
		case "message":
			if role, _ := d["role"].(string); role == "assistant" {
				if c, ok := d["content"].(string); ok && c != "" {
					pending.WriteString(c)
					pending.WriteString("\n")
				}
			}
		case "action":
			if am, ok := d["assistantMessage"].(string); ok && am != "" {
				pending.WriteString(am)
				pending.WriteString("\n")
			}
			tool, _ := d["toolName"].(string)
			if tool == "update_file" || tool == "create_file" {
				if tool == "update_file" {
					out.UpdateFileCount++
				}
				if !sawFirstUpdate && tool == "update_file" {
					text := pending.String()
					if causeLocusRe.MatchString(text) && symptomLocusRe.MatchString(text) {
						out.CauseLocusBeforeWrite = true
					}
					sawFirstUpdate = true
				}
			}
		case "observation":
			tool, _ := d["toolName"].(string)
			if tool == "run_reproducer" {
				out.HasReproducer = true
				raw, _ := d["raw_json"].(string)
				if raw == "" {
					if b, err := json.Marshal(d); err == nil {
						raw = string(b)
					}
				}
				if strings.Contains(raw, `"isError":true`) || strings.Contains(raw, `"isError": true`) {
					out.LastReproducerErrored = true
				} else {
					out.LastReproducerErrored = false
				}
			}
		}
	}
	return out
}

// patchLineCount returns the number of \n in <workDir>/patch.diff (cheap
// proxy for "lines added/removed"; correlated tightly with patch
// severity for SWE-Lite).
func patchLineCount(workDir string) int {
	b, err := os.ReadFile(filepath.Join(workDir, "patch.diff"))
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte{'\n'})
}

// -------------------------------------------------------------------
// Shared — hard revert
// -------------------------------------------------------------------

// gitCheckoutAll reverts all unstaged + staged changes in
// <workDir>/repo to the base commit. Idempotent; safe to call when
// nothing was changed. Untracked files (e.g. repro.py, the agent's
// .agent/ telemetry) are deliberately preserved — only tracked-file
// modifications are rolled back.
func gitCheckoutAll(ctx context.Context, workDir string, out *bufio.Writer) error {
	repoDir := filepath.Join(workDir, "repo")
	if st, err := os.Stat(repoDir); err != nil || !st.IsDir() {
		return fmt.Errorf("no repo/ dir at %s", repoDir)
	}
	_ = execCmd(ctx, repoDir, "git", "restore", "--staged", ".")
	if err := execCmd(ctx, repoDir, "git", "checkout", "--", "."); err != nil {
		emitMeta(out, map[string]any{
			"phase": "force_revert_error",
			"step":  "checkout",
			"error": err.Error(),
		})
		return err
	}
	return nil
}

// -------------------------------------------------------------------
// Patch 1 — Cause-locus forced re-think prompt
// -------------------------------------------------------------------

func buildCauseLocusForcePrompt(symbols []string, fileHint string) string {
	sym := "[the function named in the issue]"
	if len(symbols) > 0 {
		sym = symbols[0]
	}
	hint := ""
	if fileHint != "" {
		hint = fmt.Sprintf(
			" The file you most often read was `%s` — the cause may live in a CALLER of it or in a sibling cleanup/destroy/clear method.",
			fileHint,
		)
	}
	return fmt.Sprintf(`STOP. Your previous turn called update_file without first analyzing WHERE the bug ORIGINATES.

Bug-report sites are SYMPTOMS. The fix often lives ONE OR TWO call-chain layers UPSTREAM:
constructors, registrars, clear/reset/destroy methods, or the caller of the symptom function.

All your tracked-file changes have been REVERTED (git checkout .). The working tree is back at the base commit.

In your next response, FIRST emit a single thought block in this exact shape:

<thought>Symptom site: <FILE>:<LINE_OR_FUNCTION>. Cause site: <FILE>:<LINE_OR_FUNCTION>. Reason same/different: <one sentence>.</thought>

THEN make your first update_file. If symptom and cause are the SAME file, the sentence must justify why NO caller / NO upstream layer / NO clearing-or-cleanup path needs to change.

Target symbol from the issue: %s%s

Worked example of this pattern: bug "RangeSlider callback leaves the figure unresponsive after fig.clear()" — symptom site is RangeSlider.set_val, but cause site is Figure.clear (the grabber reference leaks when the axes is removed). Patching set_val passes ad-hoc repros but fails the hidden test that exercises clear() directly.`, sym, hint)
}

// -------------------------------------------------------------------
// Patch 2 — Forced-revert thrash prompt
// -------------------------------------------------------------------

func buildThrashRevertForcePrompt(symbols []string, prevPatchLines, prevUpdateFileCount int) string {
	sym := "[the function named in the issue]"
	if len(symbols) > 0 {
		sym = symbols[0]
	}
	return fmt.Sprintf(`STOP. You have made %d update_file calls and your patch has grown to %d lines, yet the reproducer / hidden tests still indicate a wrong fix. This is a "thrash-deeper" pattern.

ALL tracked-file changes have been REVERTED (git checkout .). The working tree is back at the base commit.

You were almost certainly fixing the WRONG LAYER of the call chain. Empirical evidence: gold patches on SWE-bench Lite average ~30 lines; the p90 is under 60 lines. Your previous attempt was sprawling at %d lines — that is a strong negative signal regardless of how much sense each individual edit made.

In your next response:
  1. Re-read the function containing %s — NOT your previous edited version, it is reverted.
  2. Look for the SMALLEST possible fix in a DIFFERENT location:
       - The caller of %s.
       - A clearing / reset / destroy / __init__ / setter / register method on the CONTAINER class.
       - A lifecycle hook on the Figure / Frame / Bp / app object, not the contained widget.
  3. Make ONE small update_file (target: under 20 lines of diff) at that alternate location.
  4. Run the reproducer to verify.

DO NOT recreate the same shape of patch. If your instinct is to write more code, the right answer is to write LESS code at a different site.`,
		prevUpdateFileCount, prevPatchLines, prevPatchLines, sym, sym)
}

// -------------------------------------------------------------------
// Convenience: short-context exec for git commands
// -------------------------------------------------------------------

func newGitCtx(parent context.Context, dur time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, dur)
}
