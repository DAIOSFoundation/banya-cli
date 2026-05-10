# Parallel B: BO@N Sample-Level Concurrency Design

**Status**: Design / pre-implementation
**Branch**: `SWE_x86` (banya-cli)
**Author**: Tony, 2026-05-10
**Constraint** (durable rule, see memory `feedback_no_cli_interference.md`): MUST NOT interfere with existing CLI features. Default behavior is unchanged sequential BO@N.

## 1. Goal & Non-Goals

**Goal**: Run the N samples of BO@N concurrently inside a single task to reduce wallclock time. Combined with Parallel A (task-level launcher in [scripts/ekto_bo2_smoke_parallel.sh](../../agent-evaluation/scripts/ekto_bo2_smoke_parallel.sh)), this gives `O(tasks × samples)` → `O(1)` speedup factor of ~6× for the standard 3-task × N=2 smoke.

**Non-goals**:
- No change to BO@N winner selection logic (`scoreBoNCandidate` runs unchanged after all samples land).
- No change to nudge / commit-force / critic-revise / pytest-revise *content* — only the workspace they execute in.
- No change to non-SWE-bench code paths (regular run, webbench, eval).
- No change to public CLI flags. New behavior gated behind `BANYA_SWE_BO_PARALLEL=1` env var (default 0 = sequential, byte-identical to current code).

## 2. The core blocker: shared workspace

Current BO@N [swe_bon.go](../cmd/banya/swe_bon.go) iterates `for i := 0; i < n; i++`:
1. `resetSWEWorkspace(workDir)` — `git reset --hard HEAD && git clean -fdx` on the single `repo/` inside `workDir`
2. Run main agent in `workDir` (creates `patch.diff`, `repro.py`, edits `repo/...`)
3. Optional nudge / commit-force / critic-revise / pytest-revise turns (also operate on `workDir`)
4. Save `patch.diff` → `patch.diff.bo<i>` for backup
5. Continue to next iteration

If we just `go func() { ... }()` the body, samples will race on:
- `resetSWEWorkspace` (sample j wipes sample i's edits mid-run)
- `patch.diff` at workspace root (each sample writes to it)
- `repo/` git state
- `.agent/trajectory.jsonl` (each sample appends)
- `repro.py` / `reproduce_issue.py` workspace files

**Bottom line**: we need per-sample workspace isolation. Goroutines without isolation = workspace corruption = invalid patches.

## 3. Workspace isolation strategy: git worktree

We use `git worktree add` to create a sample-specific workspace that **shares the underlying git object database** with the original `repo/`.

### Layout

```
<workDir>/                      # task workspace (existing)
├── patch.diff                  # winner's patch (set after BO@N completes)
├── repo/                       # canonical repo (untouched during parallel BO@N)
│   └── .git/                   # shared object DB
└── .bo/                        # NEW: per-sample isolation root (only when parallel)
    ├── 0/
    │   ├── patch.diff          # sample 0's patch
    │   ├── repro.py
    │   └── repo/               # `git worktree add ../.bo/0/repo HEAD` from canonical
    │       └── .git            # actually a file pointing at canonical/.git/worktrees/0
    └── 1/
        ├── patch.diff
        ├── repro.py
        └── repo/
```

### Setup (per-task, before goroutines)

```go
// Pseudocode
canonicalRepo := filepath.Join(workDir, "repo")
boRoot := filepath.Join(workDir, ".bo")
os.MkdirAll(boRoot, 0755)

for i := 0; i < n; i++ {
    sampleDir := filepath.Join(boRoot, strconv.Itoa(i))
    sampleRepo := filepath.Join(sampleDir, "repo")
    os.MkdirAll(sampleDir, 0755)

    // Atomic isolated worktree pointing at HEAD.
    // -B <branch>: create/reset a temporary branch (allows independent commits per sample)
    cmd := exec.Command("git", "worktree", "add", "-B",
        fmt.Sprintf("banya-bo-%d", i), sampleRepo, "HEAD")
    cmd.Dir = canonicalRepo
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("worktree %d setup failed: %w", i, err)
    }
}
```

### Teardown (per-task, after winner selection)

```go
// 1. Capture winner's patch before tearing down.
winnerPatch := filepath.Join(boRoot, strconv.Itoa(winner.Index), "patch.diff")
canonicalPatch := filepath.Join(workDir, "patch.diff")
copyFile(winnerPatch, canonicalPatch)

// 2. Backup loser trajectories before removal (existing logic uses these for SIBDD).
//    Already done during the parallel run via emitMeta — only need to copy out.

// 3. Remove worktrees.
for i := 0; i < n; i++ {
    sampleRepo := filepath.Join(boRoot, strconv.Itoa(i), "repo")
    exec.Command("git", "worktree", "remove", "--force", sampleRepo).
        CombinedOutput()  // best-effort; log failures but don't abort
    exec.Command("git", "branch", "-D", fmt.Sprintf("banya-bo-%d", i)).
        CombinedOutput()
}

// 4. Remove .bo/ root (containing winner's patch.diff backup is already captured).
os.RemoveAll(boRoot)
```

### Why git worktree (vs cp -r):
- **Disk**: shared object DB; only HEAD checkout is duplicated. matplotlib's `lib/` checkout is ~50MB. cp -r × 2 samples = 100MB extra/task. Worktree is essentially free (filesystem hardlinks for the source tree but separate working dir state).
- **Speed**: worktree add is ~200-500ms vs cp -r at 5-15s per task.
- **Cleanup**: `git worktree remove --force` is atomic; cp -r leaves orphan dirs on crash.
- **Independence**: each sample gets its own branch (`banya-bo-{i}`), so `git diff > patch.diff` produces sample-specific diff without ref collisions.

### Why NOT cp -r:
- Disk cost: 50-200MB × N samples × tasks = could blow workspace quota.
- Slow: cp -r dominates the per-task setup time, defeating the speedup goal.
- Stale state: cp -r copies the *current* state of `repo/` which may be polluted from prior runs. Worktree always starts from HEAD.

## 4. Concurrency model

```go
// Pseudocode — replaces the `for i := 0; i < n; i++` loop body.

if !parallelEnabled {
    // Existing sequential code path — unchanged.
    return runBoNSequential(...)
}

// Setup per-sample worktrees (sequential — fast).
for i := 0; i < n; i++ {
    if err := setupWorktree(canonicalRepo, boRoot, i); err != nil {
        // On worktree setup failure, fall back to sequential (loud warning).
        emitMeta(out, map[string]any{
            "phase": "swe_bo_n_parallel_fallback",
            "reason": err.Error(),
        })
        return runBoNSequential(...)
    }
}

// Spawn one goroutine per sample. Each runs the existing per-sample body
// (nudge / commit-force / critic-revise / pytest-revise) in its isolated
// worktree.
var wg sync.WaitGroup
candidates := make([]bonCandidate, n)
emitMu := sync.Mutex{}  // serialize stdout writes (jsonl line atomicity)

for i := 0; i < n; i++ {
    wg.Add(1)
    go func(idx int) {
        defer wg.Done()

        sampleDir := filepath.Join(boRoot, strconv.Itoa(idx))
        c := runSampleInWorkspace(idx, sampleDir, &emitMu, ...)
        candidates[idx] = c
    }(i)
}
wg.Wait()

// Existing winner selection logic — unchanged.
winner := selectBoNWinner(candidates)
copyWinnerArtefacts(boRoot, workDir, winner.Index)
teardownWorktrees(canonicalRepo, boRoot, n)

return winner, candidates
```

### Critical: per-sample workDir threading

Currently `runOneTurn` and `refreshPatchDiff` take `workDir` as parameter. We pass each goroutine its **sample-specific workDir** (`<original_workDir>/.bo/<i>`) so the agent loop, patch.diff writes, and `.agent/trajectory.jsonl` all stay isolated.

Functions to thread `sampleWorkDir` through (rather than the original `workDir`):
- `runOneTurn` — already takes workDir
- `refreshPatchDiff(workDir, out)` — operates on `<workDir>/patch.diff`
- `resetSWEWorkspace(workDir, out)` — would reset sample's worktree (still useful per-iteration if we keep b+ phases inside each sample's flow)
- `extractMostReadSourceFile(workDir)` — reads `.agent/trajectory.jsonl` per-sample
- `patchHasOnlySandboxFiles(patchPath)` — operates on per-sample patch.diff
- `extractPatchedFiles(patchPath)` — same
- `findTestFilesForModule(workDir, ...)` — reads sample's `repo/` for matching tests

Each sample's `.agent/trajectory.jsonl` is now under `<sampleDir>/.agent/`, so trajectory.bo<i>.jsonl backup logic just needs to copy from there to canonical workspace at end (existing pattern, just different source path).

### Synchronization points (only TWO, both narrow)

1. **Stdout writer (`out *bufio.Writer`)** — every `emitMeta` writes a JSONL line to stdout. Without a mutex, JSONL lines could interleave at byte level. Wrap `emitMeta` calls in `emitMu.Lock()/Unlock()`. The mutex contention is negligible (microsecond writes).

2. **Candidates slice** — each goroutine writes to `candidates[idx]`. Different indexes don't conflict, but we must `wg.Wait()` before reading the slice. No additional locking needed.

That's it. The agent loop, file I/O, patch.diff writes — all are workspace-local thanks to worktree isolation, no other shared state.

## 5. Opt-in mechanism

```go
// At the top of swe_bon.go's runBoN:
parallelEnabled := os.Getenv("BANYA_SWE_BO_PARALLEL") == "1"
if !parallelEnabled {
    // Existing for-loop, byte-identical to today.
    return runBoNSequential(...)
}
return runBoNParallel(...)
```

- `BANYA_SWE_BO_PARALLEL=1`: opt in to parallel mode.
- Default (unset, "0", anything else): sequential — code path is the existing for-loop verbatim.
- The launcher script (Parallel A) sets the env var when desired:
  ```bash
  export BANYA_SWE_BO_PARALLEL=1  # added to ekto_bo2_smoke_parallel.sh
  ```

No new CLI flag — env var only — to avoid touching cmd-flag plumbing in run.go (which is shared with non-SWE-bench code paths).

## 6. Concurrency safety review

| Resource | Sequential code | Parallel concern | Mitigation |
|---|---|---|---|
| `<sampleDir>/patch.diff` | overwritten per iteration | each sample has its own | worktree isolation |
| `<sampleDir>/repo/` git state | reset between iterations | each sample has its own worktree, separate branch | `git worktree add -B banya-bo-<i>` |
| `<sampleDir>/.agent/trajectory.jsonl` | appended per iteration | each sample has its own | worktree isolation (`.agent/` lives in sampleDir) |
| stdout JSONL events | sequential writes | line interleaving | `emitMu sync.Mutex` |
| `candidates []bonCandidate` | written then read in same goroutine | indexed writes from N goroutines | only different indexes; wg.Wait() before read |
| `os.Setenv("BANYA_SWE_BO_TEMPERATURE", ...)` | reset per iteration | **GLOBAL — race** | see below |
| `os.Setenv("BANYA_SWE_BO_TOP_P", ...)` | reset per iteration | **GLOBAL — race** | see below |
| LLM server | sequential requests | concurrent requests | server already supports — verified 3× concurrent |
| Critic API (Gemini) | called once per sample | N concurrent calls | API supports it; standard concurrency |
| pytest gate | runs in sample's repo dir | each sample has own repo | worktree isolation |

### The `os.Setenv` problem

Sequential BO@N sets temperature / top_p env vars per iteration:
```go
_ = os.Setenv("BANYA_SWE_BO_TEMPERATURE", fmt.Sprintf("%.3f", temp))
_ = os.Setenv("BANYA_SWE_BO_TOP_P", fmt.Sprintf("%.3f", topP))
```

These are read by the sidecar via `os.Getenv` to pass to the LLM. **Two parallel goroutines clobbering global env = wrong temperatures.**

**Fix**: thread temperature/top_p through `protocol.ChatRequest` directly. Add fields to ChatRequest (or use existing model-config fields) so each request carries its own temp/topP. Remove the env var hack.

This is a minor protocol change. Check pkg/protocol/types.go for existing model-config wiring — if absent, add `Temperature *float64` and `TopP *float64` to the request struct. Sidecar reads them when present, falls back to env vars when nil (preserving existing behavior for non-parallel mode).

## 7. Testing plan

### Phase 1 — sequential mode regression test

Run the existing 3-task smoke set with `BANYA_SWE_BO_PARALLEL` UNSET. Expected: byte-identical patches and scores to the v18.1 baseline run. Any difference = bug in code that wasn't supposed to change.

### Phase 2 — parallel mode correctness test

Run the same 3-task smoke set with `BANYA_SWE_BO_PARALLEL=1`. Expected:
- Per-sample trajectories logically equivalent to sequential (different timing, similar tool sequences)
- Final patch.diff = winning sample's patch
- BO@N scoring decisions match (within stochastic tolerance — temperature spread is the same)
- `swe_bo_n_done` event emitted correctly with `winner_index`

Compare task-by-task: `pass@1` should match sequential within stochastic noise. Differences in patches between the two modes are EXPECTED (different timing → different sampling) but the SCORES per task should be statistically indistinguishable.

### Phase 3 — wallclock smoke

Measure end-to-end wallclock time for a 3-task smoke. Expected:
- Sequential: ~3-4h (current)
- Parallel A only: ~75min (max-of-tasks)
- Parallel A + B: ~40min (max-of-tasks ÷ 2)

### Phase 4 — failure-mode tests

- Worktree setup failure (e.g., disk full): expect `swe_bo_n_parallel_fallback` meta event + sequential mode continues task.
- One sample crashes mid-run: other samples should complete; winner picked from survivors.
- LLM server overload (concurrent burst exceeds capacity): expect rate-limit retries within sample, no cross-sample contamination.

## 8. Migration path

1. **PR1**: Add `Temperature` / `TopP` fields to `protocol.ChatRequest`. Sidecar reads them when set. Backwards-compatible (nil → env var fallback).
2. **PR2**: Implement `runBoNParallel` alongside `runBoNSequential`. Top-level `runBoN` dispatches via env var. Both paths exercised by tests.
3. **PR3**: Add `BANYA_SWE_BO_PARALLEL=1` to `ekto_bo2_smoke_parallel.sh`. Smoke against the 3-task set.
4. **PR4**: Default to parallel mode after 2-3 successful smoke runs. Remove env var gate (becomes `BANYA_SWE_BO_PARALLEL=0` for opt-out instead).

PR1+2 = code change. PR3 = launcher tweak. PR4 = config flip after validation.

## 9. Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Worktree setup fails on first task | Low | High (smoke aborted) | fallback to sequential — already designed |
| Worktree teardown fails after winner | Low | Medium (orphan dirs accumulate) | best-effort + log; cleanup on next run start |
| LLM server can't sustain N concurrent requests | Low | Medium (latency spike) | already verified 3× concurrent; cap N or add per-task throttle |
| Stdout JSONL interleaving breaks tools.jsonl parsing | Med | Low | mutex around emitMeta — designed |
| Different temperatures across parallel samples ≠ sequential reproducibility | Cert | None (we're stochastic anyway) | document as expected; sequential mode preserved as-is |
| Worktree state leaks across BO@N runs (different tasks) | Med | High (cross-task contamination) | always teardown before next task; idempotent setup |
| `os.Setenv` race on temperature | Cert if not fixed | High | thread temp/topP through ChatRequest — see §6 |

## 10. Estimated effort

- PR1 (ChatRequest fields + sidecar wiring): ~3-4h
- PR2 (`runBoNParallel` + worktree setup/teardown): ~6-8h
- PR3 (launcher integration): ~1h
- PR4 (default flip + cleanup): ~1h after 2-3 validated runs

Total: ~10-13h of focused work + ~2-3 smoke runs to validate.

## 11. Implementation discovery — concurrency obstacle in `ProcessClient`

(Added 2026-05-10 during pre-implementation walkthrough.)

`internal/client/process.go` exposes `SendMessage(req)` which returns a **shared event channel** `c.events` (line 443) — every concurrent `SendMessage` call multiplexes into the same channel. There is no per-session demultiplexing in `ProcessClient`. `runOneTurn` iterates `for evt := range events` without filtering on `evt.SessionID`, so two goroutines calling `runOneTurn` concurrently would steal each other's events.

This means raw goroutine BO@N is unsafe with the current `ProcessClient`. Three ways forward:

**Option C-event-filter** (intrusive — touches shared client code): Add a per-session event subscription API (e.g., `pc.SubscribeSession(id) <-chan ServerEvent`) that demultiplexes `c.events` into per-session channels. Risk: `process.go` is shared with non-SWE-bench code paths (regular run, webbench). Adding a fanout dispatcher requires careful regression testing on those. Violates "no interference" constraint unless implemented additively (existing `SendMessage` keeps current shared-channel behavior; new method is additive).

**Option C-process** (process-isolated — recommended): Spawn `N` child `banya` processes per task. Each child gets:
- its own sidecar (process boundary kills shared-state risk)
- its own workspace (worktree as designed in §3)
- its own ChatRequest invocation
- its own stdout JSONL (parent collects from temp files)

Parent process orchestrates: spawns children, waits, reads each child's `patch.diff` + scoring metadata, applies `scoreBoNCandidate` to pick winner, copies winner's patch to canonical workspace. Children run UNCHANGED sequential code (N=1) — no banya-cli code edits to internal call paths.

Cost: heavier (each child loads ~125MB sidecar binary, ~1-2s startup). For N=2, that's 2-4s overhead per task.

**Option C-stub** (current implementation): worktree helpers + dispatch flag, but the body still runs sequentially. Documents the approach without unsafe goroutines. Provides an upgrade path for v2 (Option C-process or C-event-filter).

## 12. v1 Implementation Decision

**Adopting Option C-stub for v1.**

Rationale:
- Smoke is currently running; we can't risk a broken Parallel B that requires another full test cycle
- Worktree setup/teardown is the foundational piece; getting that right de-risks all future implementations
- True wallclock speedup needs Option C-process; that's a 4-6h focused implementation we can do in a fresh session with proper review
- Parallel A alone already gives 3× speedup; the additional 2× from Parallel B is icing
- "No interference with existing CLI" rules out goroutine-with-shared-event-channel implementation

v1 deliverable:
- `BANYA_SWE_BO_PARALLEL=1` env var detection in `runBoN`
- Worktree setup + teardown helpers (tested + ready to use)
- A clear meta event `swe_bo_n_parallel_unsupported` when env var is set, indicating fallback to sequential
- Design notes recorded here for v2 implementation

v2 plan (next iteration, separate PR):
- Choose between Option C-event-filter and Option C-process based on user preference
- Full implementation + smoke validation
- Default flip after 2-3 successful smokes

## 13. Decision points needed before v2

1. **Worktree branch naming**: `banya-bo-<i>` vs `banya-bo-<task_id>-<i>`? Latter is safer (can't collide across parallel tasks if same workDir somehow), but workDir is per-task anyway so `banya-bo-<i>` should be fine.

2. **Worktree backup strategy**: when `BANYA_SWE_BO_KEEP_LOSERS=1`, current logic backs up `patch.diff` → `patch.diff.bo<i>` AT WORKSPACE ROOT. With per-sample workspaces, do we (a) copy each sample's patch.diff to `<workDir>/patch.diff.bo<i>` (preserving current contract for SIBDD downstream), or (b) leave them in `<workDir>/.bo/<i>/patch.diff` and update SIBDD pair extractor to read from there? Recommend (a) — minimal SIBDD change.

3. **Worktree cleanup on crash**: if banya-cli crashes mid-BO@N, worktrees remain. Should banya-cli scan for orphan `<workDir>/.bo/` dirs at startup and clean them? Recommend yes — single line in main.go.

4. **Concurrency cap**: do we let users set `BANYA_SWE_BO_PARALLEL_MAX=4` to cap concurrent samples (when N is large)? Probably not worth complexity for current N=2 smoke. Leave as future work if N=16 BO@N becomes operational.
