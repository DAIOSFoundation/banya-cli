// swe_bon_parallel.go — C-process implementation of Parallel B.
//
// Spawns one child banya-cli process per BO@N sample. Each child runs
// the regular sequential flow (run.go's `runE`) inside its own
// per-sample workspace (a git worktree under <workDir>/.bo/<idx>/),
// emits a `bo_meta.json` at exit, and the parent collects+ranks them
// using the existing scoreBoNCandidate function.
//
// Why C-process (vs goroutines):
// `internal/client/process.go` SendMessage returns a SHARED event
// channel `c.events`. Two goroutines calling SendMessage concurrently
// would steal each other's events. Implementing safe per-session
// event demultiplexing in ProcessClient is a moderate refactor that
// touches non-SWE-bench code paths (regular run, webbench). C-process
// achieves identical wallclock parallelism with full state isolation
// (separate sidecar per child) without touching shared client code.
// See docs/PARALLEL_B_DESIGN.md §11 for the full discussion.
//
// Adheres to feedback_no_cli_interference.md: parent's default code
// path (env var unset) is byte-identical to v18.1; the child code
// path is reached only when BANYA_SWE_BO_CHILD_INDEX is set in env,
// which only happens via parent's spawn.

package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// boChildMeta is the structured result emitted by a child process at
// exit (written to <sampleWorkDir>/bo_meta.json). The parent reads
// this to construct a bonCandidate and apply scoreBoNCandidate.
//
// Fields mirror bonCandidate's verifier signals, deliberately
// JSON-tagged so the schema is stable across child / parent versions.
type boChildMeta struct {
	Index         int    `json:"index"`
	HasPatch      bool   `json:"has_patch"`
	PatchHash     string `json:"patch_hash"`
	PatchPath     string `json:"patch_path"`     // absolute path inside the child's workdir
	NudgeFired    bool   `json:"nudge_fired"`
	CommitForced  bool   `json:"commit_forced"`  // always false for now (child runs regular flow, no commit-force)
	CriticRan     bool   `json:"critic_ran"`
	CriticOK      bool   `json:"critic_ok"`
	CriticRevised bool   `json:"critic_revised"`
	PytestRan     bool   `json:"pytest_ran"`
	PytestPass    bool   `json:"pytest_pass"`
	PytestRevised bool   `json:"pytest_revised"`
	Notes         string `json:"notes,omitempty"`
	WallElapsedS  int    `json:"wall_elapsed_s"`
}

// writeBoChildMeta is called at the very end of regular-run flow in
// run.go (only when BANYA_SWE_BO_CHILD_INDEX is set). It serialises
// the verifier signals collected during the child's run to a stable
// JSON file the parent can read.
//
// Returns nil on best-effort success; failures are logged via emitMeta
// but never abort the child (child must exit cleanly so parent's
// cmd.Wait() returns).
func writeBoChildMeta(out *bufio.Writer, workDir string, m boChildMeta) error {
	path := filepath.Join(workDir, "bo_meta.json")
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		emitMeta(out, map[string]any{
			"phase":  "swe_bo_child_meta_write_error",
			"error":  err.Error(),
		})
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		emitMeta(out, map[string]any{
			"phase":  "swe_bo_child_meta_write_error",
			"error":  err.Error(),
			"path":   path,
		})
		return err
	}
	emitMeta(out, map[string]any{
		"phase":      "swe_bo_child_meta_written",
		"index":      m.Index,
		"path":       path,
		"has_patch":  m.HasPatch,
		"critic_ok":  m.CriticOK,
		"pytest_pass": m.PytestPass,
	})
	return nil
}

// readBoChildMeta is the parent-side counterpart. Returns the meta
// struct or an error if the file is missing / malformed (which means
// the child crashed before writing it — parent treats this as
// has_patch=false, score=0 for that sample).
func readBoChildMeta(path string) (boChildMeta, error) {
	var m boChildMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// runBoNViaChildren is the parent-side orchestrator that spawns
// `n` child banya-cli processes (one per BO@N sample), waits for all
// of them, and returns the same (winner, candidates) shape as the
// sequential runBoN's tail.
//
// Caller (in runBoN) is expected to handle the dispatch:
//
//	if os.Getenv("BANYA_SWE_BO_PARALLEL") == "1" && os.Getenv("BANYA_SWE_BO_CHILD_INDEX") == "" {
//	    return runBoNViaChildren(...)
//	}
func runBoNViaChildren(
	ctx context.Context,
	out *bufio.Writer,
	sessionID string,
	workDir, patchPath string,
	n int,
	tempMin, tempMax float64,
	perChildTimeout time.Duration,
	keepLosers bool,
) (winner *bonCandidate, candidates []bonCandidate) {
	// stdoutMu serializes our parent-side stdout writes. emitMeta is
	// called from this goroutine + child-stdout-relay goroutines.
	var stdoutMu sync.Mutex
	emit := func(payload map[string]any) {
		stdoutMu.Lock()
		defer stdoutMu.Unlock()
		emitMeta(out, payload)
	}

	emit(map[string]any{
		"phase":             "swe_bo_n_parallel_start",
		"version":           "v2_c_process",
		"n":                 n,
		"session_id":        sessionID,
		"per_child_timeout": int(perChildTimeout.Seconds()),
	})

	// Pre-flight: clean any orphan worktrees from a crashed prior run.
	cleanupOrphanWorktrees(workDir, out)

	// Setup phase — sequential (worktree creation is fast).
	sampleDirs := make([]string, n)
	for i := 0; i < n; i++ {
		sd, err := setupSampleWorktree(workDir, i)
		if err != nil {
			emit(map[string]any{
				"phase":  "swe_bo_n_parallel_setup_error",
				"index":  i,
				"error":  err.Error(),
			})
			// Tear down any worktrees we already created before bailing.
			for j := 0; j < i; j++ {
				teardownSampleWorktree(workDir, j, out)
			}
			return nil, nil
		}
		sampleDirs[i] = sd
		emit(map[string]any{
			"phase":      "swe_bo_n_parallel_worktree_ready",
			"index":      i,
			"sample_dir": sd,
		})
	}

	// Spawn N children in parallel.
	type childResult struct {
		idx     int
		meta    boChildMeta
		err     error
		exitErr string
	}
	resultCh := make(chan childResult, n)
	var wg sync.WaitGroup

	executable := os.Args[0]
	// Strip the leading subcommand argv from the parent invocation; the
	// child needs the same subcommand + flags to run the same task.
	childArgs := append([]string{}, os.Args[1:]...)
	// Force --swe-bo-n 1 in the child so it skips the BO@N branch and
	// runs run.go's regular flow (agent → nudge → critic → pytest).
	childArgs = injectBoNOne(childArgs)
	// Force --timeout to match sequential BO@N's per-sample budget.
	// Without this, child uses banya-cli's 600s default (HALF of
	// sequential), times out in the middle of long agent loops.
	// Astropy needs ~2000s per BO@N sample; we use perChildTimeout
	// directly which already includes effectiveTimeout + nudgeTimeout.
	childArgs = injectTimeout(childArgs, perChildTimeout)

	startedAt := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			temp := tempMin
			if n > 1 {
				temp = tempMin + (tempMax-tempMin)*float64(idx)/float64(n-1)
			}
			topP := 0.9 + 0.05*float64(idx)/float64(maxIntLocal(1, n-1))

			sampleDir := sampleDirs[idx]
			logPath := filepath.Join(sampleDir, "child_stdout.jsonl")
			errPath := filepath.Join(sampleDir, "child_stderr.log")

			logF, lerr := os.Create(logPath)
			if lerr != nil {
				resultCh <- childResult{idx: idx, err: fmt.Errorf("create log: %w", lerr)}
				return
			}
			defer logF.Close()
			errF, eerr := os.Create(errPath)
			if eerr != nil {
				resultCh <- childResult{idx: idx, err: fmt.Errorf("create errlog: %w", eerr)}
				return
			}
			defer errF.Close()

			// Build command. We pass --workspace <sampleDir> so run.go
			// chdirs into the per-sample workspace.
			argsWithWS := append(append([]string{}, childArgs...), "--workspace", sampleDir)
			cctx, ccancel := context.WithTimeout(ctx, perChildTimeout)
			defer ccancel()
			cmd := exec.CommandContext(cctx, executable, argsWithWS...)
			cmd.Stdout = logF
			cmd.Stderr = errF
			// Per-child env: clear PARALLEL flag so child doesn't try to
			// spawn grandchildren; set CHILD_INDEX so child knows to write
			// bo_meta.json at exit; set per-sample temperature/top_p.
			cmd.Env = append(os.Environ(),
				"BANYA_SWE_BO_PARALLEL=",
				fmt.Sprintf("BANYA_SWE_BO_CHILD_INDEX=%d", idx),
				fmt.Sprintf("BANYA_SWE_BO_TEMPERATURE=%.3f", temp),
				fmt.Sprintf("BANYA_SWE_BO_TOP_P=%.3f", topP),
			)

			emit(map[string]any{
				"phase":       "swe_bo_n_parallel_child_spawn",
				"index":       idx,
				"sample_dir":  sampleDir,
				"temperature": temp,
				"top_p":       topP,
			})

			runErr := cmd.Run()
			exitErrText := ""
			if runErr != nil {
				exitErrText = runErr.Error()
			}

			// Read child's bo_meta.json. Best-effort — if missing, the
			// child crashed and we score it as has_patch=false.
			metaPath := filepath.Join(sampleDir, "bo_meta.json")
			meta, merr := readBoChildMeta(metaPath)
			if merr != nil {
				meta = boChildMeta{Index: idx, Notes: fmt.Sprintf("bo_meta.json missing: %v", merr)}
			}
			meta.Index = idx
			resultCh <- childResult{idx: idx, meta: meta, err: runErr, exitErr: exitErrText}
		}(i)
	}

	wg.Wait()
	close(resultCh)
	wallElapsed := time.Since(startedAt)

	// Collect results.
	results := make([]childResult, 0, n)
	for r := range resultCh {
		results = append(results, r)
	}

	// Build candidates slice in index order.
	candidates = make([]bonCandidate, n)
	for _, r := range results {
		c := bonCandidate{
			Index:         r.idx,
			HasPatch:      r.meta.HasPatch,
			PatchHash:     r.meta.PatchHash,
			PatchPath:     r.meta.PatchPath, // path in sample dir
			NudgeFired:    r.meta.NudgeFired,
			CommitForced:  r.meta.CommitForced,
			CriticRan:     r.meta.CriticRan,
			CriticOK:      r.meta.CriticOK,
			CriticRevised: r.meta.CriticRevised,
			PytestRan:     r.meta.PytestRan,
			PytestPass:    r.meta.PytestPass,
			PytestRevised: r.meta.PytestRevised,
			Notes:         r.meta.Notes,
		}
		if c.Notes == "" && r.exitErr != "" {
			c.Notes = "child_exit_error: " + r.exitErr
		}
		c.Score = scoreBoNCandidate(c)

		// If the child has a patch but we don't have a hash, compute it now
		// from the sample's patch.diff.
		if c.HasPatch && c.PatchHash == "" {
			samplePatch := filepath.Join(sampleDirs[r.idx], "patch.diff")
			if data, err := os.ReadFile(samplePatch); err == nil {
				h := sha1.Sum(data)
				c.PatchHash = hex.EncodeToString(h[:])[:12]
			}
		}
		candidates[r.idx] = c

		emit(map[string]any{
			"phase":          "swe_bo_n_sample_done",
			"index":          c.Index,
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
			"parent_mode":    "c_process",
		})

		// If keepLosers, copy the child's trajectory.jsonl to canonical
		// .agent/trajectory.bo<i>.jsonl for SIBDD downstream.
		if keepLosers {
			src := filepath.Join(sampleDirs[r.idx], ".agent", "trajectory.jsonl")
			dst := filepath.Join(workDir, ".agent", fmt.Sprintf("trajectory.bo%d.jsonl", r.idx))
			if err := copyFileForBackup(src, dst); err == nil {
				emit(map[string]any{
					"phase":      "swe_bo_n_trajectory_backup",
					"index":      r.idx,
					"path":       dst,
					"session_id": sessionID,
				})
			}
		}

		// Also copy the child's stdout.jsonl into .agent for debugging.
		stdoutSrc := filepath.Join(sampleDirs[r.idx], "child_stdout.jsonl")
		stdoutDst := filepath.Join(workDir, ".agent", fmt.Sprintf("child_stdout.bo%d.jsonl", r.idx))
		_ = copyFileForBackup(stdoutSrc, stdoutDst)
	}

	// Pick winner using the SAME logic as sequential runBoN.
	sortedCandidates := append([]bonCandidate{}, candidates...)
	sortBoNCandidates(sortedCandidates)
	if len(sortedCandidates) > 0 && sortedCandidates[0].HasPatch {
		w := sortedCandidates[0]
		// Copy winner's patch.diff to canonical workdir.
		winnerPatch := filepath.Join(sampleDirs[w.Index], "patch.diff")
		if err := copyFileForBackup(winnerPatch, patchPath); err != nil {
			emit(map[string]any{
				"phase":  "swe_bo_n_parallel_winner_copy_error",
				"error":  err.Error(),
				"index":  w.Index,
			})
		} else {
			w.PatchPath = patchPath
		}
		winner = &w
	}

	// Emit aggregate done event matching sequential runBoN's format.
	emit(map[string]any{
		"phase":              "swe_bo_n_done",
		"n":                  n,
		"winner_index":       optInt(winner, -1),
		"winner_score":       optScore(winner),
		"winner_pytest":      optPytest(winner),
		"winner_critic":      optCritic(winner),
		"session_id":         sessionID,
		"parent_mode":        "c_process",
		"wall_elapsed_s":     int(wallElapsed.Seconds()),
	})

	// Cleanup worktrees. Best-effort.
	for i := 0; i < n; i++ {
		teardownSampleWorktree(workDir, i, out)
	}

	return winner, candidates
}

// sortBoNCandidates applies the same ordering as sequential runBoN's
// inline sort.SliceStable — pulled out so both code paths share a
// single source of truth for winner selection.
func sortBoNCandidates(cs []bonCandidate) {
	// Stable sort: highest score first, pytest pass tiebreaker, then index.
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && bonCandidateLess(cs[j], cs[j-1]); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

func bonCandidateLess(a, b bonCandidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.PytestPass != b.PytestPass {
		return a.PytestPass
	}
	return a.Index < b.Index
}

// injectBoNOne ensures `--swe-bo-n 1` is set on the child invocation
// so the child runs run.go's regular flow rather than re-entering BO@N.
// Strips any existing `--swe-bo-n <N>` pair (or `--swe-bo-n=N` form)
// and appends the corrected pair at the end.
func injectBoNOne(args []string) []string {
	out := make([]string, 0, len(args)+2)
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--swe-bo-n" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "--swe-bo-n=") {
			continue
		}
		out = append(out, a)
	}
	out = append(out, "--swe-bo-n", "1")
	return out
}

// injectTimeout ensures the child uses a generous per-turn `--timeout`
// instead of the banya-cli default (600s, which is half of what
// sequential BO@N's per-sample-timeout uses). Without this fix,
// child banya-cli running run.go's regular flow would hit per-turn
// timeout in the middle of long agent loops (observed empirically:
// astropy v18.2 child timed out at 35min while sequential v18
// astropy needed 67min to finish).
//
// Strips any existing `--timeout <D>` pair (or `--timeout=D` form)
// and appends the corrected one. The format must be a duration string
// (e.g., `1200s`) parseable by Cobra's GetDuration.
func injectTimeout(args []string, timeout time.Duration) []string {
	out := make([]string, 0, len(args)+2)
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--timeout" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "--timeout=") {
			continue
		}
		out = append(out, a)
	}
	out = append(out, "--timeout", timeout.String())
	return out
}

// maxIntLocal — local helper to avoid namespace clash with maxInt in swe_bon.go
// (they're identical; this is here so this file is self-contained).
func maxIntLocal(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// (used by callers in this file to keep io error handling small)
var _ = io.EOF
var _ = strconv.Itoa
