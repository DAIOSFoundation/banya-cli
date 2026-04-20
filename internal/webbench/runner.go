package webbench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// TestResult captures the outcome of a cumulative Playwright run —
// parallels what Web-Bench's official evaluator records after each
// task: total assertions, how many passed, and the failure tail so the
// agent can act on it.
type TestResult struct {
	AllPassed     bool
	PassedCount   int
	FailedCount   int
	FailedSpecs   []string      // distinct task-N.spec.js names that had at least one failure
	Stdout        string
	Stderr        string
	ExitCode      int
	Duration      time.Duration
	TimedOut      bool
}

// RunCumulative invokes `npx playwright test` for all spec paths in
// `specs`. Mirrors Web-Bench's `npm run test -- N` which runs
// `playwright test test/task-1.spec … test/task-N.spec` so a later
// task's implementation can be caught regressing on an earlier spec.
//
// The command is run with `IS_EVAL_PRODUCTION=1` so Web-Bench's
// playwright.config.js uses the 30s / 5s timeouts (vs 2s / 1s in
// its dev mode) that match the official evaluator.
func RunCumulative(ctx context.Context, workDir string, specs []string, timeout time.Duration) (TestResult, error) {
	if len(specs) == 0 {
		return TestResult{AllPassed: true}, nil
	}
	if _, err := exec.LookPath("npx"); err != nil {
		return TestResult{}, fmt.Errorf("npx not on PATH — install Node.js v22+")
	}

	args := append([]string{"playwright", "test"}, specs...)
	args = append(args, "--reporter=line")

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "npx", args...)
	cmd.Dir = workDir
	env := os.Environ()
	env = append(env, "IS_EVAL_PRODUCTION=1")
	cmd.Env = env

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	res := TestResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: dur,
	}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		res.ExitCode = -1
	}

	res.PassedCount, res.FailedCount, res.FailedSpecs = parsePlaywrightLineReport(res.Stdout)
	res.AllPassed = !res.TimedOut && res.ExitCode == 0 && res.FailedCount == 0
	return res, nil
}

// playwrightLineHeaderRe matches the summary line at the end of a
// `--reporter=line` run — e.g. "5 passed (4.0s)" or "2 failed".
var (
	passCountRe = regexp.MustCompile(`(?m)^\s*(\d+)\s+passed\b`)
	failCountRe = regexp.MustCompile(`(?m)^\s*(\d+)\s+failed\b`)
	failSpecRe  = regexp.MustCompile(`›\s*(test/task-\d+\.spec\.js)`)
)

// parsePlaywrightLineReport extracts passed/failed counts and the
// distinct failing spec file names from Playwright's line reporter
// output. The format is stable across 1.48-1.56.
func parsePlaywrightLineReport(out string) (passed, failed int, failedSpecs []string) {
	if m := passCountRe.FindStringSubmatch(out); m != nil {
		fmt.Sscanf(m[1], "%d", &passed)
	}
	if m := failCountRe.FindStringSubmatch(out); m != nil {
		fmt.Sscanf(m[1], "%d", &failed)
	}
	seen := map[string]bool{}
	for _, m := range failSpecRe.FindAllStringSubmatch(out, -1) {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			failedSpecs = append(failedSpecs, name)
		}
	}
	return passed, failed, failedSpecs
}

// BuildNudge formats a continuation prompt that feeds failing Playwright
// output back to the agent. Parallels SWE-bench's buildNudgePrompt()
// in cmd/banya/run.go: very directive ("STOP / FIX / REGENERATE") so
// the agent doesn't fall back into exploration.
//
// When `allSpecs` is true the nudge reminds the agent that earlier
// specs must keep passing (the cumulative gating rule). Useful for
// subsequent tasks where regressing task-1 is a real failure mode.
func BuildNudge(result TestResult, allSpecs bool) string {
	tail := tailLines(result.Stdout+"\n"+result.Stderr, 40)
	failSummary := strings.Join(result.FailedSpecs, ", ")
	if failSummary == "" {
		failSummary = "(multiple tests)"
	}

	var b strings.Builder
	b.WriteString("STOP. Your previous changes broke Playwright tests. ")
	b.WriteString(fmt.Sprintf("Passed %d, failed %d (%s).\n\n", result.PassedCount, result.FailedCount, failSummary))
	b.WriteString("Fix the implementation in src/ so ALL failing specs pass")
	if allSpecs {
		b.WriteString(" — and keep every earlier spec green (Web-Bench's cumulative gating rule).\n\n")
	} else {
		b.WriteString(".\n\n")
	}
	b.WriteString("Required steps:\n")
	b.WriteString("1. Open the failing spec file and identify the exact assertions/selectors that fail.\n")
	b.WriteString("2. Read the current src/ code you wrote and find the mismatch (classNames, structure, state, routing, ref timing).\n")
	b.WriteString("3. Make the minimum edits that satisfy the failing assertions AND do not break earlier specs.\n")
	b.WriteString("4. Do NOT rewrite `package.json`, `vite.config.*`, or `playwright.config.*`.\n\n")
	b.WriteString("### Test output (tail)\n```\n")
	b.WriteString(tail)
	if !strings.HasSuffix(tail, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	return b.String()
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
