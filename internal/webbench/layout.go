// Package webbench wraps the Web-Bench "cumulative Playwright test +
// failing-spec nudge" flow as a run.go companion to the SWE-bench
// patch.diff + critic pattern.
//
// Layout probe finds `test/task-N.spec.js` files + a playwright-enabled
// `package.json` in the working dir. When both are present, run.go
// activates the webbench post-turn path: execute cumulative tests up to
// the highest detected spec, and on failure, feed the playwright output
// back to the agent for one fix-up turn (mirroring the SWE-bench
// "commit the patch" nudge).
package webbench

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Layout describes a detected Web-Bench-style workspace.
type Layout struct {
	// WorkDir is the root we inspected — caller's cwd at probe time.
	WorkDir string
	// SpecFiles is task-1.spec.js → task-N.spec.js ordered by N, only
	// counting the ones that actually exist on disk. Non-contiguous
	// ranges are allowed; the runner iterates this list verbatim.
	SpecFiles []string
	// MaxIndex is the highest N we saw in a task-N.spec.js file name.
	MaxIndex int
	// HasPlaywrightConfig is true when `playwright.config.js` or .ts is
	// present. Absent config usually means the project is not a
	// Web-Bench workspace even if a spec dir exists.
	HasPlaywrightConfig bool
	// NodeModulesPresent indicates whether `npx playwright` is likely
	// to find its deps. When false the runner will still try, but the
	// caller can choose to warn the user first.
	NodeModulesPresent bool
	// CurrentIndex is the task number the orchestrator says we're
	// currently implementing (1-based). Set via the env var
	// BANYA_WEBBENCH_CURRENT_INDEX — benchmarks/harnesses fill it in
	// before each request. Zero means "unknown" and disables Active().
	CurrentIndex int
}

// Active reports whether the workspace looks like Web-Bench AND the
// orchestrator supplied a concrete current-task index. Without the
// index we can't scope the cumulative test to task-1..N and would
// end up running all 20 specs on what the agent has only made one
// attempt at — 19 nominal failures that drown the nudge prompt.
// The index arrives via `BANYA_WEBBENCH_CURRENT_INDEX`; interactive
// sessions without it fall through to either npm-test general mode
// or no-test mode.
func (l Layout) Active() bool {
	return len(l.SpecFiles) > 0 && l.HasPlaywrightConfig && l.CurrentIndex > 0
}

// taskSpecPattern matches `test/task-N.spec.js` (banya-cli currently
// only consumes JS specs; the Web-Bench react project ships JS even
// though the src is TS).
var taskSpecPattern = regexp.MustCompile(`^task-(\d+)\.spec\.js$`)

// Detect probes workDir for a Web-Bench-style layout.
func Detect(workDir string) Layout {
	layout := Layout{WorkDir: workDir}

	testDir := filepath.Join(workDir, "test")
	entries, err := os.ReadDir(testDir)
	if err == nil {
		type idxed struct {
			idx  int
			name string
		}
		found := make([]idxed, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			m := taskSpecPattern.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			n, _ := strconv.Atoi(m[1])
			found = append(found, idxed{idx: n, name: e.Name()})
		}
		sort.Slice(found, func(i, j int) bool { return found[i].idx < found[j].idx })
		for _, f := range found {
			layout.SpecFiles = append(layout.SpecFiles, filepath.Join("test", f.name))
			if f.idx > layout.MaxIndex {
				layout.MaxIndex = f.idx
			}
		}
	}

	for _, name := range []string{"playwright.config.js", "playwright.config.ts", "playwright.config.mjs"} {
		if _, err := os.Stat(filepath.Join(workDir, name)); err == nil {
			layout.HasPlaywrightConfig = true
			break
		}
	}

	if st, err := os.Stat(filepath.Join(workDir, "node_modules")); err == nil && st.IsDir() {
		layout.NodeModulesPresent = true
	} else if st, err := os.Lstat(filepath.Join(workDir, "node_modules")); err == nil && st.Mode()&os.ModeSymlink != 0 {
		// symlinked node_modules (webbench rush install layout) —
		// same deal, deps are reachable even if Stat on the link
		// target fails due to dangling symlink.
		layout.NodeModulesPresent = true
	}

	if raw := strings.TrimSpace(os.Getenv("BANYA_WEBBENCH_CURRENT_INDEX")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if layout.MaxIndex > 0 && n > layout.MaxIndex {
				n = layout.MaxIndex
			}
			layout.CurrentIndex = n
		}
	}

	return layout
}

// SpecRange returns the slice of spec rel-paths up to and including the
// given task index. Caller chooses the cut — usually `MaxIndex` for a
// full cumulative run, or a lower number for incremental checks.
// Missing specs in the range are silently dropped (same as the CLI-side
// `npm run test -- N` behaviour in Web-Bench's test.sh).
func (l Layout) SpecRange(endIdx int) []string {
	if endIdx <= 0 {
		return nil
	}
	out := make([]string, 0, len(l.SpecFiles))
	for _, p := range l.SpecFiles {
		base := filepath.Base(p)
		m := taskSpecPattern.FindStringSubmatch(base)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n <= endIdx {
			out = append(out, p)
		}
	}
	return out
}

// DescribeShort renders a one-liner for log/meta events.
func (l Layout) DescribeShort() string {
	b := strings.Builder{}
	b.WriteString("webbench layout: ")
	b.WriteString(strconv.Itoa(len(l.SpecFiles)))
	b.WriteString(" specs, max=task-")
	b.WriteString(strconv.Itoa(l.MaxIndex))
	if l.HasPlaywrightConfig {
		b.WriteString(", +pw-config")
	}
	if l.NodeModulesPresent {
		b.WriteString(", +node_modules")
	}
	return b.String()
}
