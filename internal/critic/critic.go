// Package critic implements a Gemini-backed patch reviewer grounded in
// deterministic tool evidence, not just language reasoning.
//
// Design (from "결정론적 Critic 시스템 설계" research, 2025):
//   1. g / h separation — this package is the Gap Detector (`g`), the
//      main agent (Actor) owns patch generation (`h`).
//   2. Context isolation — Critic sees only the final patch + issue +
//      tool evidence. The Actor's chain-of-thought is never shared.
//   3. Deterministic evidence — before calling the LLM we gather:
//        (a) file context (modified files' full content)
//        (b) reproducer execution (extract ```python blocks from the
//            issue and run them in the repo; stdout/stderr are the
//            ground truth)
//        (c) static analysis (ruff) on patched files
//   4. Structured Gap Object — LLM emits JSON with 4-dim orthogonal
//      rubric scores, severity, and a typed list of issues. The caller
//      can gate on severity without reparsing prose.
//   5. 9-section prompt with an internal <thinking> multi-expert debate
//      before the final JSON verdict.
package critic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta/models"
	DefaultModel    = "gemini-3-flash-preview"
	defaultTimeout  = 120 * time.Second
)

// Scores is the 4-dimensional orthogonal rubric. Each dimension is
// scored 0-25, total 0-100. Keeps dimensions truly independent so a
// strong score on one doesn't mask a weak score on another.
type Scores struct {
	Correctness   int `json:"correctness"`    // Does patch implement the fix?
	Completeness  int `json:"completeness"`   // All cases (edge/example) covered?
	CodeQuality   int `json:"code_quality"`   // Style, readability, conventions
	BestPractices int `json:"best_practices"` // Error handling, OOP, docs
}

// Total returns the sum of the four dimensions (0-100).
func (s Scores) Total() int {
	return s.Correctness + s.Completeness + s.CodeQuality + s.BestPractices
}

// Issue is one concrete, evidence-bound defect report. Empty when
// severity is low — Critic is asked to skip nits.
type Issue struct {
	Type     string  `json:"type"`     // Logical | Syntax | Performance | Security | Style
	Location string  `json:"location"` // file:line (or file:range)
	Evidence string  `json:"evidence"` // concrete observation (test failure, ruff line, …)
	Critique string  `json:"critique"` // one-sentence why-wrong
	Severity float64 `json:"severity"` // 0-1
}

// GapObject is the full parsed verdict from a single critic call.
type GapObject struct {
	OK            bool    `json:"ok"`             // true = no revise needed
	OverallSeverity float64 `json:"overall_severity"` // 0-1; used for early stop
	Summary       string  `json:"summary"`        // one-line human-readable
	Scores        Scores  `json:"scores"`
	Issues        []Issue `json:"issues"`
	ReviseDirectives []string `json:"revise_directives,omitempty"` // actionable bullets for Actor
	Raw           string  `json:"-"` // full LLM response for debugging
}

// Decision mirrors the legacy struct for back-compat with callers that
// only care about the OK / Feedback pair.
type Decision struct {
	OK       bool
	Feedback string
	Gap      *GapObject // nil if parse failed
	Raw      string
}

type Reviewer struct {
	// Provider runs the actual model call. Legacy callers that set
	// APIKey/Model/Endpoint still work — NewFromEnv populates Provider
	// with a GeminiProvider built from those fields. Callers wiring up
	// Claude Code (or anything else) just assign Provider directly.
	Provider CriticProvider

	// Legacy Gemini fields — kept so existing tests / callers that
	// construct &Reviewer{APIKey: …} continue to compile. When Provider
	// is non-nil these are ignored.
	APIKey   string
	Model    string
	Endpoint string
	Timeout  time.Duration
	// DomainTier — optional, set from BANYA_DOMAIN_TIER. When "low" /
	// "none" we raise the PASS threshold because we have less prior
	// information about what a "good" patch looks like in that slice.
	// Empty = use defaults (85/22/18).
	DomainTier string
}

// PassThreshold is the minimum (total / correctness / completeness /
// maxSeverity) a patch needs to hit for critic to emit ok=true. It
// tightens with lower domain confidence.
type PassThreshold struct {
	Total        int     // minimum aggregate score
	Correctness  int
	Completeness int
	MaxSeverity  float64 // ok=false when any issue's severity exceeds this
}

// thresholdFor returns the PASS gate for a given domain tier. Low-
// confidence tiers tighten the gate so the critic biases toward REVISE
// in domains where our priors are weak.
func thresholdFor(tier string) PassThreshold {
	switch tier {
	case "none":
		return PassThreshold{Total: 92, Correctness: 24, Completeness: 21, MaxSeverity: 0.5}
	case "low":
		return PassThreshold{Total: 90, Correctness: 23, Completeness: 20, MaxSeverity: 0.55}
	default: // high / medium / "" (legacy)
		return PassThreshold{Total: 85, Correctness: 22, Completeness: 18, MaxSeverity: 0.6}
	}
}

// NewFromEnv builds a Reviewer from env. Returns nil when no provider
// can be constructed. Provider selection order:
//
//	BANYA_CRITIC_PROVIDER=claude-code → ClaudeCodeProvider
//	BANYA_CRITIC_PROVIDER=gemini       → GeminiProvider (default)
//	<unset>                            → GeminiProvider (backward compat)
//
// Per-provider env:
//	claude-code: BANYA_CRITIC_CLAUDE_BIN, BANYA_CRITIC_MODEL (alias)
//	gemini:      BANYA_CRITIC_API_KEY | BANYA_SUBAGENT_API_KEY | GEMINI_KEY,
//	             BANYA_CRITIC_MODEL | BANYA_SUBAGENT_MODEL,
//	             BANYA_CRITIC_ENDPOINT | BANYA_SUBAGENT_ENDPOINT
func NewFromEnv() *Reviewer {
	providerKind := os.Getenv("BANYA_CRITIC_PROVIDER")
	tier := os.Getenv("BANYA_DOMAIN_TIER")

	if providerKind == "claude-code" {
		model := firstNonEmpty(os.Getenv("BANYA_CRITIC_MODEL"), "sonnet")
		bin := os.Getenv("BANYA_CRITIC_CLAUDE_BIN")
		return &Reviewer{
			Provider:   NewClaudeCodeProvider(bin, model, defaultTimeout),
			Timeout:    defaultTimeout,
			DomainTier: tier,
		}
	}

	// Default / "gemini" path.
	apiKey := firstNonEmpty(
		os.Getenv("BANYA_CRITIC_API_KEY"),
		os.Getenv("BANYA_SUBAGENT_API_KEY"),
		os.Getenv("GEMINI_KEY"),
	)
	if apiKey == "" {
		return nil
	}
	model := firstNonEmpty(
		os.Getenv("BANYA_CRITIC_MODEL"),
		os.Getenv("BANYA_SUBAGENT_MODEL"),
		DefaultModel,
	)
	endpoint := firstNonEmpty(
		os.Getenv("BANYA_CRITIC_ENDPOINT"),
		os.Getenv("BANYA_SUBAGENT_ENDPOINT"),
		DefaultEndpoint,
	)
	return &Reviewer{
		Provider:   NewGeminiProvider(apiKey, model, endpoint, defaultTimeout),
		APIKey:     apiKey,
		Model:      model,
		Endpoint:   endpoint,
		Timeout:    defaultTimeout,
		DomainTier: tier,
	}
}

// ReviewPatch gathers deterministic evidence (file context + reproducer
// run + static analysis) and asks the LLM to emit a structured GapObject.
func (r *Reviewer) ReviewPatch(ctx context.Context, issue, patch, repoRoot string) (Decision, error) {
	if r == nil {
		return Decision{OK: true, Feedback: "(no reviewer configured)"}, nil
	}

	// Collect deterministic evidence first. Each helper is best-effort;
	// failures degrade the Critic's evidence but don't block the call.
	fileContext := ""
	reproducerEvidence := "(no reproducer in issue)"
	staticEvidence := "(no static analysis run)"
	regressionEvidence := "(no related tests run)"
	if repoRoot != "" {
		fileContext = gatherFileContext(patch, repoRoot)
		if repro := runReproducer(ctx, issue, repoRoot); repro != "" {
			reproducerEvidence = repro
		}
		if lint := runRuff(ctx, patch, repoRoot); lint != "" {
			staticEvidence = lint
		}
		if regr := runRelatedTests(ctx, patch, repoRoot); regr != "" {
			regressionEvidence = regr
		}
	}

	threshold := thresholdFor(r.DomainTier)
	prompt := buildReviewPrompt(issue, patch, fileContext, reproducerEvidence, staticEvidence, regressionEvidence, r.DomainTier, threshold)

	// Resolve the provider. Legacy callers that built &Reviewer{APIKey:...}
	// still work — we construct a GeminiProvider on the fly.
	provider := r.Provider
	if provider == nil {
		if r.APIKey == "" {
			return Decision{OK: false, Feedback: "(no critic provider / api key)"}, nil
		}
		provider = NewGeminiProvider(r.APIKey, r.Model, r.Endpoint, r.Timeout)
	}

	raw, err := provider.Review(ctx, ReviewArgs{
		ReviewPrompt: prompt,
		Issue:        issue,
		Patch:        patch,
		RepoRoot:     repoRoot,
		DomainTier:   r.DomainTier,
	})
	if err != nil {
		// Transport / provider failure. Do NOT default to ok=true — that
		// was the silent bug v6-strat's matplotlib-18869 exposed (gemini
		// empty reply → critic rubber-stamped the wrong patch). Bias to
		// REVISE so the agent gets another round instead of a false OK.
		return Decision{
			OK:       false,
			Feedback: "(critic provider err: " + truncate(err.Error(), 120) + ")",
		}, err
	}
	if strings.TrimSpace(raw) == "" {
		// Same rule: empty response → REVISE, not OK.
		return Decision{
			OK:       false,
			Feedback: "(critic empty response — treating as REVISE)",
		}, nil
	}

	gap, err := parseGapObject(raw)
	if err != nil {
		// Parse failed — again REVISE by default (previously we defaulted
		// to OK which let malformed responses pass the gate). Surface the
		// raw output so logs still carry signal.
		return Decision{
			OK:       false,
			Feedback: "(critic parse err: " + truncate(err.Error(), 80) + ")",
			Raw:      raw,
		}, nil
	}
	feedback := gap.Summary
	if !gap.OK && len(gap.ReviseDirectives) > 0 {
		feedback += " | " + strings.Join(gap.ReviseDirectives, "; ")
	}
	return Decision{OK: gap.OK, Feedback: feedback, Gap: gap, Raw: raw}, nil
}

// buildReviewPrompt assembles the 9-section enterprise prompt. Sections
// map to the structure from the research doc: Role / Objective / Context
// / Evidence / Rubric / Reasoning / Patch Guide / Output / Termination.
func buildReviewPrompt(
	issue, patch, fileContext, reproducerEvidence, staticEvidence, regressionEvidence string,
	domainTier string,
	threshold PassThreshold,
) string {
	const issueCap = 8000
	const patchCap = 12000
	const fileCtxCap = 20000
	const evidenceCap = 6000
	if len(issue) > issueCap {
		issue = issue[:issueCap] + "...[truncated]"
	}
	if len(patch) > patchCap {
		patch = patch[:patchCap] + "...[truncated]"
	}
	if len(fileContext) > fileCtxCap {
		fileContext = fileContext[:fileCtxCap] + "\n...[truncated]"
	}
	if len(reproducerEvidence) > evidenceCap {
		reproducerEvidence = reproducerEvidence[:evidenceCap] + "\n...[truncated]"
	}
	if len(staticEvidence) > evidenceCap {
		staticEvidence = staticEvidence[:evidenceCap] + "\n...[truncated]"
	}
	if len(regressionEvidence) > evidenceCap {
		regressionEvidence = regressionEvidence[:evidenceCap] + "\n...[truncated]"
	}
	if fileContext == "" {
		fileContext = "(no modified file content collected)"
	}

	return fmt.Sprintf(`## §1 Role & Identity
You are a senior software engineer + strict security reviewer with 20 years of experience auditing Python codebases. Your sole job: catch patches that LOOK plausible from the diff but FAIL to fix the issue. Confirmation bias is your enemy — assume the patch is wrong until deterministic evidence proves otherwise.

## §2 Objective & Scope
Review ONE SWE-bench patch against ONE issue. Decide accept / revise. Do NOT propose unrelated refactors, style improvements, or alternative designs. Scope is strictly "does this diff correctly fix this bug?".

## §3 Context & Constraints
- You have one shot — no follow-up tools or clarifications.
- Default stance: REVISE unless you can verify all four rubric dimensions from the artefacts alone.
- A symptom-mask (e.g. try/except swallowing the error, special-case for the example input only) is REVISE even if "it works".
- Missing reproducer coverage or failing static check = REVISE.

## §4 Evidence (deterministic, from tools — this is ground truth)

### §4.1 ISSUE TEXT
%s

### §4.2 PATCH (unified diff)
%s

### §4.3 FULL CONTENT OF MODIFIED FILES (post-patch, for ±full-file context beyond the ±3 diff window)
%s

### §4.4 REPRODUCER EXECUTION (issue's example code run against post-patch repo)
%s

### §4.5 STATIC ANALYSIS (ruff on modified files)
%s

### §4.6 RELATED PRE-EXISTING TESTS
Two signals combined:
  (a) EXECUTED — pytest was run on repo-native tests that already cover the patched module(s). A failing exit is a HARD regression signal → REVISE regardless of diff aesthetics.
      ⚠️ EXCEPTION: if the section header says "SKIPPED pytest (environment-issue)" or the NOTE line explicitly says the failure is NOT a regression (e.g. ModuleNotFoundError, ImportError on conftest, collected 0 items, missing repo deps), IGNORE it — the host environment isn't set up to run the suite, and this tells you NOTHING about the patch. Do not REVISE solely on this signal.
  (b) READ-ONLY SPECS — when patched files are .ts/.tsx/.js/.jsx, we dump the full content of co-located *.spec.* / *.test.* files (Playwright / Jest / Vitest). These test bodies ARE the behavioral spec. Mentally run each assertion against the patched code — if any assertion would fail, this is REVISE.
%s

## §5 Multi-Dimensional Rubric (each 0-25, orthogonal, total 0-100)
- **correctness** (0-25): does the patch actually fix the described bug? 25 = reproducer passes with expected output; 10 = reproducer still fails; 0 = patch doesn't touch the buggy code path.
- **completeness** (0-25): are all edge cases mentioned or implied by the issue handled? Missing None/empty/boundary = -10 each. **Hidden test suites in SWE-bench-style benchmarks are strict — a "looks right on the happy path" patch that ignores boundary inputs loses here even when correctness is high. Be skeptical.**
- **code_quality** (0-25): does the code read well, use clear names, match surrounding style?
- **best_practices** (0-25): proper error handling, no broad except, preserved function signatures, docstrings intact.

PASS threshold (dynamic — tightens as domain confidence drops; see §5b for the tier that applies to THIS task):
  - total ≥ %[7]d, AND
  - correctness ≥ %[8]d, AND
  - completeness ≥ %[9]d, AND
  - no Severity > %[10].2f issue, AND
  - at least one of {reproducer EXECUTED and passed, related-tests EXECUTED and passed} — when both are env-issue/absent, default to REVISE unless the diff is a textbook single-point fix with full docstring/signature preservation.

When in doubt → REVISE. False negatives (flagging a good patch) cost one extra revise round; false positives (approving a broken patch) cost an entire task.

## §5b Domain-tier context applied to THIS review
- Active tier: **%[11]s** (set by banya-cli from a static workspace scan; empty = legacy/unclassified; treat as "medium").
- Thresholds above were scaled for this tier (low/none tighten them because our priors are weaker).
- When tier is low/none, do NOT rubber-stamp plausible-looking diffs: the agent is operating outside its measured surface, so hidden-test regressions are MORE likely than on in-distribution work.

## §6 Reasoning Protocol (mandatory before the JSON verdict)
Think step-by-step inside a <thinking> block. Within <thinking> you MUST simulate a four-expert panel debating the patch:
- **CS-Theorist**: walks the reproducer input through the patched code line by line. Does the expected output emerge?
- **Edge-Case Hunter**: lists 3 inputs not in the issue but plausible (None, empty, nested, boundary, negative, very large). Would the patch handle them?
- **Security Auditor**: checks if the patch widens an attack surface or introduces silent failures.
- **Maintenance Reviewer**: checks signature / return / docstring preservation.
Have each expert give a verdict + one sentence. Resolve disagreement by evidence (prefer tool output over intuition). Close the <thinking> block before emitting JSON.

## §7 Patch Guidance (populate only if ok=false)
For each revise_directive: be specific (file:line), concrete ("change X to Y because Z"), and MINIMAL. Do not suggest rewrites. Keep the patch gradable — one directive per actionable change.

## §8 Output Schema
Emit ONLY a JSON object matching this shape. No markdown fences, no commentary outside JSON:
{
  "ok": boolean,
  "overall_severity": 0.0 to 1.0,
  "summary": "one-sentence verdict",
  "scores": {"correctness": 0-25, "completeness": 0-25, "code_quality": 0-25, "best_practices": 0-25},
  "issues": [{"type":"Logical|Syntax|Performance|Security|Style", "location":"file:line", "evidence":"concrete observation", "critique":"one sentence", "severity":0.0-1.0}],
  "revise_directives": ["file:line — change X to Y because Z", ...]
}

## §9 Termination Heuristics
- ok=true REQUIRES ALL of: total ≥ %[7]d, correctness ≥ %[8]d, completeness ≥ %[9]d, no Severity > %[10].2f issue, AND at least one positive execution signal (reproducer passed OR related-tests passed). When both execution signals are env-issue/absent, default to ok=false UNLESS the diff is a textbook single-point fix (signature/docstring preserved, one line changed in a function body, addresses the issue symbol directly).
- If reproducer evidence shows a failure, ok MUST be false regardless of diff aesthetics.
- If ruff shows an error-level finding in modified files, ok MUST be false.
- If the Edge-Case Hunter expert (see §6) flags any of the three probes as a likely miss, ok MUST be false.
- issues array may be empty when ok=true; revise_directives may be empty when ok=true.
`, issue, patch, fileContext, reproducerEvidence, staticEvidence, regressionEvidence,
		threshold.Total, threshold.Correctness, threshold.Completeness, threshold.MaxSeverity,
		func() string {
			if domainTier == "" {
				return "medium"
			}
			return domainTier
		}())
}

// parseGapObject accepts either a bare JSON object or one wrapped in a
// ```json ... ``` fence. Unknown fields are tolerated; missing required
// fields fall back to safe defaults (ok=true, summary="…").
func parseGapObject(raw string) (*GapObject, error) {
	s := strings.TrimSpace(raw)
	// Strip markdown fence if present.
	if strings.HasPrefix(s, "```") {
		// Drop first fence line.
		nl := strings.Index(s, "\n")
		if nl > 0 {
			s = s[nl+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	// If there's leading prose, extract the first balanced {...}.
	if start := strings.Index(s, "{"); start >= 0 && start > 0 {
		s = s[start:]
	}

	var gap GapObject
	if err := json.Unmarshal([]byte(s), &gap); err != nil {
		return nil, err
	}
	if gap.Summary == "" {
		if gap.OK {
			gap.Summary = "OK to commit"
		} else {
			gap.Summary = "REVISE"
		}
	}
	gap.Raw = raw
	return &gap, nil
}

// ─────────────────────────── evidence helpers ───────────────────────────

// gatherFileContext unchanged from earlier revision — reads modified
// files' full content and joins them with separators.
func gatherFileContext(patch, repoRoot string) string {
	if patch == "" || repoRoot == "" {
		return ""
	}
	paths := extractPatchPaths(patch)
	if len(paths) == 0 {
		return ""
	}
	const perFileCap = 10000
	var b strings.Builder
	for _, rel := range paths {
		full := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			alt := filepath.Join(repoRoot, "repo", rel)
			data, err = os.ReadFile(alt)
			if err != nil {
				continue
			}
		}
		content := string(data)
		if len(content) > perFileCap {
			content = content[:perFileCap] + "\n...[truncated " + fmt.Sprint(len(data)-perFileCap) + " bytes]"
		}
		b.WriteString("----- ")
		b.WriteString(rel)
		b.WriteString(" -----\n")
		b.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

var (
	reDiffGit    = regexp.MustCompile(`^diff --git a/(\S+) b/(\S+)`)
	rePlusFile   = regexp.MustCompile(`^\+\+\+ b/(\S+)`)
	rePyFence    = regexp.MustCompile("(?ms)```(?:python|py)?\\s*\\n(.*?)```")
	rePyFromLine = regexp.MustCompile(`(?m)^(>>> |\.\.\. )?(from |import )`)
)

// extractPatchPaths unchanged.
func extractPatchPaths(patch string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		var path string
		if m := reDiffGit.FindStringSubmatch(line); m != nil {
			path = m[2]
		} else if m := rePlusFile.FindStringSubmatch(line); m != nil {
			path = m[1]
		}
		if path == "" || path == "/dev/null" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

// runReproducer looks for the first ```python ... ``` block in the
// issue (or the first plausible >>> transcript) and executes it inside
// repoRoot. stdout/stderr/exit are formatted so the LLM has concrete
// evidence of "did the patch actually work".
//
// Why this matters (the central insight from CRITIC, 2023): a patch can
// be lexically plausible and still miss the issue's example. The only
// way to check is to *run* the example. Same-model critics anchored on
// the diff alone are easy fooled — execution grounds them.
func runReproducer(ctx context.Context, issue, repoRoot string) string {
	code := extractReproducer(issue)
	if code == "" {
		return ""
	}
	// Prefer to run inside repo/ (SWE-bench layout) — that's where the
	// agent placed its fix via update_file.
	cwd := filepath.Join(repoRoot, "repo")
	if _, err := os.Stat(cwd); err != nil {
		cwd = repoRoot
	}
	// Hard cap to avoid runaway infinite loops or heavy imports.
	runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "python3", "-c", code)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		exitCode = -1
	}
	return fmt.Sprintf(
		"$ python3 -c '<issue reproducer>' (cwd=%s)\n"+
			"[exit=%d]\n"+
			"--- STDOUT ---\n%s\n"+
			"--- STDERR ---\n%s",
		cwd, exitCode, truncate(stdout.String(), 3000), truncate(stderr.String(), 3000))
}

// extractReproducer pulls the first executable Python block from the
// issue text. Priority:
//   1. ```python``` fenced block containing `import` / `from`
//   2. first ``` fenced block (language-agnostic) if it mentions import/from
//   3. >>> transcript lines stitched into a script
// Returns "" if no runnable block was found.
func extractReproducer(issue string) string {
	for _, m := range rePyFence.FindAllStringSubmatch(issue, -1) {
		block := strings.TrimSpace(m[1])
		if block == "" {
			continue
		}
		if rePyFromLine.MatchString(block) || strings.Contains(block, "def ") || strings.Contains(block, "class ") {
			// Strip >>> / ... prompts if present (pasted REPL transcripts).
			return stripReplPrompts(block)
		}
	}
	return ""
}

func stripReplPrompts(code string) string {
	lines := strings.Split(code, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(trim, ">>> "):
			out = append(out, strings.TrimPrefix(trim, ">>> "))
		case strings.HasPrefix(trim, "... "):
			out = append(out, strings.TrimPrefix(trim, "... "))
		case trim == ">>>" || trim == "...":
			// skip empty prompt line
		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// runRuff invokes `ruff check` on the patched files and returns its
// output as evidence. When ruff isn't installed we silently skip —
// static analysis is a bonus signal, not a blocker.
func runRuff(ctx context.Context, patch, repoRoot string) string {
	paths := extractPatchPaths(patch)
	if len(paths) == 0 {
		return ""
	}
	// Only keep python files — ruff is python-specific.
	var pyPaths []string
	for _, p := range paths {
		if strings.HasSuffix(p, ".py") {
			pyPaths = append(pyPaths, p)
		}
	}
	if len(pyPaths) == 0 {
		return ""
	}
	cwd := filepath.Join(repoRoot, "repo")
	if _, err := os.Stat(cwd); err != nil {
		cwd = repoRoot
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := append([]string{"check", "--output-format=concise"}, pyPaths...)
	cmd := exec.CommandContext(runCtx, "ruff", args...)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		// ruff not installed or timed out — skip quietly.
		return ""
	}
	body := out.String()
	if exit == 0 && body == "" {
		body = "(no ruff findings)"
	}
	return fmt.Sprintf("$ ruff check %s (cwd=%s)\n[exit=%d]\n%s",
		strings.Join(pyPaths, " "), cwd, exit, truncate(body, 4000))
}

// runRelatedTests combines two regression signals:
//
//  1. EXECUTED pytest — for every patched .py file we look for the repo's
//     pre-existing test_<basename>.py under conventional locations and
//     run pytest against them. A failure catches the "fix works on the
//     reproducer but breaks something else" class of bad patches.
//
//  2. READ-ONLY frontend/e2e specs — for patched .ts/.tsx/.js/.jsx we
//     locate co-located *.spec.* / *.test.* files (Playwright / Jest /
//     Vitest convention) and dump their CONTENT into the evidence. We
//     don't execute them (headless browser / node test harness is too
//     heavy for a 60s critic budget), but the test file body itself is
//     a declarative behavioral spec the critic can reason over directly.
//
// Either signal alone is valuable, so we emit whatever we have. Returns
// "" when neither applies (e.g. patch only touches docs, or the repo has
// no tests co-located with the patched module).
func runRelatedTests(ctx context.Context, patch, repoRoot string) string {
	paths := extractPatchPaths(patch)
	if len(paths) == 0 {
		return ""
	}
	cwd := filepath.Join(repoRoot, "repo")
	if _, err := os.Stat(cwd); err != nil {
		cwd = repoRoot
	}

	var sections []string
	if tests := findPytestPaths(paths, cwd); len(tests) > 0 {
		if out := runPytest(ctx, tests, cwd); out != "" {
			sections = append(sections, out)
		}
	}
	if specs := readFrontendSpecs(paths, cwd); specs != "" {
		sections = append(sections, specs)
	}
	if len(sections) == 0 {
		return ""
	}
	return strings.Join(sections, "\n\n")
}

// findPytestPaths searches conventional locations for pytest files that
// cover each patched .py module. Bounded to 3 matches to keep runtime
// predictable.
func findPytestPaths(patchPaths []string, cwd string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, rel := range patchPaths {
		if !strings.HasSuffix(rel, ".py") {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(rel), ".py")
		if base == "" || strings.HasPrefix(base, "test_") {
			continue
		}
		dir := filepath.Dir(rel)
		parent := filepath.Dir(dir)
		candidates := []string{
			filepath.Join(dir, "tests", "test_"+base+".py"),
			filepath.Join(dir, "test_"+base+".py"),
			filepath.Join(dir, "tests", base+"_test.py"),
			filepath.Join("tests", "test_"+base+".py"),
			filepath.Join("tests", dir, "test_"+base+".py"),
		}
		if parent != "" && parent != "." && parent != "/" {
			candidates = append(candidates,
				filepath.Join(parent, "tests", "test_"+base+".py"),
				filepath.Join(parent, "test_"+base+".py"),
			)
		}
		for _, c := range candidates {
			if _, ok := seen[c]; ok {
				continue
			}
			full := filepath.Join(cwd, c)
			if st, err := os.Stat(full); err == nil && !st.IsDir() {
				seen[c] = struct{}{}
				out = append(out, c)
				if len(out) >= 3 {
					return out
				}
			}
		}
	}
	return out
}

// runPytest invokes pytest against the given relative test paths. -x
// stops at first failure so output stays small. Missing pytest is a
// silent skip — not every environment ships it.
//
// Environment-classification heuristic (critical): SWE-bench repos
// checked out on the host rarely have their dependencies installed —
// pytest immediately fails with ModuleNotFoundError / ImportError /
// "collected 0 items". If we forwarded that raw failure, the critic
// would read it as a regression and REVISE a patch that is actually
// fine. Instead, detect those markers and label the section
// "environment-issue" so the prompt's §4.6 rule can ignore the run.
func runPytest(ctx context.Context, tests []string, cwd string) string {
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	args := append([]string{"-x", "--tb=short", "--no-header", "-q"}, tests...)
	cmd := exec.CommandContext(runCtx, "pytest", args...)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		// pytest not installed or binary missing — skip quietly.
		return ""
	}
	body := out.String()
	envIssue := classifyPytestEnvIssue(body, exit)
	if exit == 0 && strings.TrimSpace(body) == "" {
		body = "(all related tests passed)"
	}
	label := "EXECUTED pytest"
	preface := ""
	if envIssue != "" {
		label = "SKIPPED pytest (environment-issue)"
		preface = fmt.Sprintf(
			"NOTE: this failure is NOT a regression — it comes from the host environment (%s). "+
				"The critic MUST NOT count it against the patch.\n",
			envIssue,
		)
	}
	return fmt.Sprintf("--- %s ---\n%s$ pytest -x --tb=short %s (cwd=%s)\n[exit=%d]\n%s",
		label, preface, strings.Join(tests, " "), cwd, exit, truncate(body, 5000))
}

// classifyPytestEnvIssue returns a short reason when pytest output looks
// like an environment/setup failure rather than a real test failure.
// Returns "" when the output can be trusted as a regression signal.
func classifyPytestEnvIssue(body string, exit int) string {
	if exit == 0 {
		return ""
	}
	lc := body
	// Fast substring checks — avoid regex overhead.
	switch {
	case strings.Contains(lc, "ModuleNotFoundError"):
		return "ModuleNotFoundError (deps not installed)"
	case strings.Contains(lc, "ImportError while loading conftest"),
		strings.Contains(lc, "ImportError: cannot import"),
		strings.Contains(lc, "ImportError: No module named"):
		return "ImportError on collection (deps / path issue)"
	case strings.Contains(lc, "collected 0 items"):
		return "collected 0 items (test discovery failed)"
	case strings.Contains(lc, "ERROR: file or directory not found"):
		return "test file path not present in checkout"
	case strings.Contains(lc, "no module named"):
		return "missing module (lower-case form)"
	case strings.Contains(lc, "error: pytest"):
		return "pytest invocation error (plugin / option)"
	case exit == 4:
		// pytest usage error — almost always environment.
		return "pytest exit=4 (usage error)"
	}
	return ""
}

// readFrontendSpecs pulls the full contents of co-located test spec
// files for patched JS/TS sources. Useful for Playwright / Jest / Vitest
// repos where the spec file itself encodes the expected behavior.
//
// We DON'T run these — headless browser tests + playwright install would
// blow past the critic's time budget. A read is still valuable: the
// critic can match each `expect(...)` against the patched code and flag
// patches that would fail the assertion.
func readFrontendSpecs(patchPaths []string, cwd string) string {
	frontendExt := map[string]bool{".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true}
	type cand struct{ rel string }
	seen := make(map[string]struct{})
	var picked []cand

	for _, rel := range patchPaths {
		ext := filepath.Ext(rel)
		if !frontendExt[ext] {
			continue
		}
		// Skip spec files themselves — we want specs that test sources.
		stem := strings.TrimSuffix(filepath.Base(rel), ext)
		if strings.HasSuffix(stem, ".spec") || strings.HasSuffix(stem, ".test") {
			continue
		}
		dir := filepath.Dir(rel)
		parent := filepath.Dir(dir)

		// Conventional suffixes (Playwright uses .spec, Jest/Vitest use either).
		suffixes := []string{".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
			".test.ts", ".test.tsx", ".test.js", ".test.jsx"}
		// Conventional test dirs.
		dirs := []string{
			dir,
			filepath.Join(dir, "__tests__"),
			filepath.Join(dir, "__test__"),
			parent,
			filepath.Join(parent, "__tests__"),
			"tests",
			filepath.Join("tests", dir),
			"e2e",
			filepath.Join("tests", "e2e"),
			filepath.Join("e2e", dir),
			"test",
		}
		for _, d := range dirs {
			for _, suf := range suffixes {
				c := filepath.Join(d, stem+suf)
				if _, ok := seen[c]; ok {
					continue
				}
				full := filepath.Join(cwd, c)
				if st, err := os.Stat(full); err == nil && !st.IsDir() {
					seen[c] = struct{}{}
					picked = append(picked, cand{rel: c})
					if len(picked) >= 4 {
						break
					}
				}
			}
			if len(picked) >= 4 {
				break
			}
		}
		if len(picked) >= 4 {
			break
		}
	}

	if len(picked) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("--- READ-ONLY frontend/e2e specs ---\n")
	b.WriteString("(these test files are behavioral contracts — the patch must satisfy every assertion)\n")
	const perFileCap = 4000
	for _, p := range picked {
		data, err := os.ReadFile(filepath.Join(cwd, p.rel))
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > perFileCap {
			content = content[:perFileCap] + "\n...[truncated " + fmt.Sprint(len(data)-perFileCap) + " bytes]"
		}
		b.WriteString("\n===== ")
		b.WriteString(p.rel)
		b.WriteString(" =====\n")
		b.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// firstNonEmpty returns the first argument that is not the empty
// string — used across NewFromEnv to cascade through preferred env vars
// with backward-compatible fallbacks.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
