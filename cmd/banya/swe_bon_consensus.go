package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Self-consistency weighting for BO@N winner selection — Wang et al. 2022
// adapted from CoT-SC voting to BO@N archive clustering.
//
// Motivation (paper §9.9, v5 astropy-12907 evidence): the verifier's hybrid
// score is (pytest×10 + critic×5 + has_patch×1). Inference-time pytest
// cannot see the hidden bug-validating tests; the critic's syntactic-quality
// bias picks elaborate reproducer-only patches over minimal canonical fixes.
// Result: archive contains the correct fix (8/10 patches edit
// astropy/modeling/separable.py, including bo2's 500-byte canonical
// `_cstack` correction) but the winner is bo12 (reproducer scripts only,
// 0 source edits). Hidden tests FAIL.
//
// The signal we ARE under-using: edit-target consensus across the BO@N
// archive. With temperature 0.7–1.0, 16 children explore different
// hypotheses; correct fixes cluster on the right file/region, mis-fires
// scatter. Multiplying the hybrid score by the size fraction of a
// candidate's edit cluster down-weights outliers (reproducer-only,
// sandbox-only, wrong-file fabrications) without touching the well-tuned
// inner score formula.
//
// Activated by `BANYA_SWE_BO_CONSENSUS_WEIGHTING=1` (default off: zero
// impact on existing v18.x experiments). Granularity selectable via
// `BANYA_SWE_BO_CONSENSUS_GRANULARITY` (default `"file"`; `"line"` /
// `"function"` reserved for follow-up).

// consensusWeightingEnabled returns true when the env var is set to "1".
func consensusWeightingEnabled() bool {
	return os.Getenv("BANYA_SWE_BO_CONSENSUS_WEIGHTING") == "1"
}

// consensusGranularity returns the granularity setting; defaults to "file".
func consensusGranularity() string {
	g := os.Getenv("BANYA_SWE_BO_CONSENSUS_GRANULARITY")
	if g == "" {
		return "file"
	}
	return g
}

// isConsensusSandbox returns true when a diff-path corresponds to a
// non-source artefact (reproducer, ad-hoc test file, plan/patch scratch).
// Diff paths from `extractPatchedFiles` are relative to repo root (the
// existing `isSandboxPath` in swe_bon.go also handles workspace-root
// paths and isn't directly reusable here).
func isConsensusSandbox(p string) bool {
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	p = strings.TrimPrefix(p, "repo/")
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
	// Any file under an in-repo test directory.
	if strings.Contains("/"+p, "/tests/") || strings.Contains("/"+p, "/test/") {
		return true
	}
	// `.backup` / `.bak` / `.orig` suffixes — agents sometimes write the
	// modified source as `<file>.backup` instead of replacing the original.
	for _, suf := range []string{".backup", ".bak", ".orig"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// editedSourceFiles returns the set of non-sandbox files a candidate's patch
// edits. Sandbox paths (repro.py, test_*.py, .backup files, /tests/) are
// excluded — they're the patches §9.9 punishes, so they should not pull
// consensus weight toward themselves OR contribute to other clusters.
// Returns nil when the patch is missing/empty/unparseable.
func editedSourceFiles(c bonCandidate) map[string]struct{} {
	if !c.HasPatch || c.PatchPath == "" {
		return nil
	}
	files := extractPatchedFiles(c.PatchPath)
	if len(files) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(files))
	for _, p := range files {
		if isConsensusSandbox(p) {
			continue
		}
		// Normalize: strip leading "repo/" so worktree vs canonical paths
		// land in the same cluster.
		p = strings.TrimPrefix(p, "repo/")
		out[p] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// computeConsensusWeight returns the fraction of archive candidates whose
// edited-source-file set overlaps with the target's. Range [0.0, 1.0].
//
//   - Degenerate archive (≤1 candidate): returns 1.0 (no-op).
//   - Target has no real source edits (sandbox-only / no patch): returns 0.0.
//   - Otherwise: |{s in archive : edits_overlap(s, target)}| / |archive|.
//
// The target itself counts when it has real edits, guaranteeing weight ≥ 1/N.
func computeConsensusWeight(target bonCandidate, archive []bonCandidate) float64 {
	if len(archive) <= 1 {
		return 1.0
	}
	targetFiles := editedSourceFiles(target)
	if len(targetFiles) == 0 {
		return 0.0
	}
	overlapping := 0
	for _, s := range archive {
		sFiles := editedSourceFiles(s)
		if len(sFiles) == 0 {
			continue
		}
		for f := range targetFiles {
			if _, ok := sFiles[f]; ok {
				overlapping++
				break
			}
		}
	}
	return float64(overlapping) / float64(len(archive))
}

// applyConsensusWeighting computes a per-candidate consensus weight, scales
// the integer hybrid Score by it, and writes the rescaled value back into
// Score so the existing sort comparator (Score > Score, PytestPass, Index)
// keeps the same tiebreaker semantics. The unscaled value is preserved in
// ScoreRaw for logging.
//
// Scale factor 1000: hybrid score range is 0–16; multiplied by weight ∈
// [0,1] gives [0, 16]. Rounding to int with ×1000 preserves three decimals
// of weight resolution — enough to distinguish 1/16 (0.0625) from 0/16
// (0.0) while keeping the comparator integer-clean.
func applyConsensusWeighting(candidates []bonCandidate) {
	if !consensusWeightingEnabled() {
		return
	}
	if len(candidates) == 0 {
		return
	}
	for i := range candidates {
		w := computeConsensusWeight(candidates[i], candidates)
		candidates[i].ConsensusWeight = w
		candidates[i].ScoreRaw = candidates[i].Score
		weighted := float64(candidates[i].Score) * w
		candidates[i].Score = int(math.Round(weighted * 1000))
	}
}
