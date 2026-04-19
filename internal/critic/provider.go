// Provider abstraction for patch-review backends.
//
// The existing `Reviewer` struct hard-codes a Gemini REST call. Now we
// also want to reach `claude` (the Claude Code CLI) as a subprocess —
// that's a fundamentally different transport, so we factor out the
// "given a prompt + evidence, return a Decision" step behind a small
// interface. The rest of critic.go (evidence gathering, prompt
// building, GapObject parsing) stays identical for both providers.
//
// Provider registry:
//
//   gemini        — REST call to generativelanguage.googleapis.com
//                   (the original, unchanged behaviour)
//   claude-code   — spawn the local `claude -p --bare --output-format json`
//                   CLI; we hand it the review prompt + workspace access
//                   so it can run pytest / ruff itself and return a
//                   GapObject by JSON schema
//
// Selection: `NewReviewerFromEnv()` reads BANYA_CRITIC_PROVIDER; empty
// falls back to gemini for backward-compat.

package critic

import "context"

// CriticProvider runs the LLM round of a review. It is given the fully
// assembled review prompt (with evidence inlined) and must return the
// raw text the model produced; critic.go then parses that into a
// GapObject and a Decision.
//
// Providers that can validate structured output (Gemini JSON mime type,
// Claude Code `--json-schema`) are encouraged to do so — the parser in
// critic.go is tolerant but emits fewer "parse failed" fallbacks when
// the response is already JSON.
type CriticProvider interface {
	// Name is used in meta events + logs. Short, lowercase.
	Name() string
	// Review returns the raw model output. issueText/patchText/reviewPrompt
	// are passed for providers that need them separately (e.g. Claude
	// Code uses issue+patch as context and reviewPrompt as the user
	// turn); most providers will simply send `reviewPrompt`.
	Review(
		ctx context.Context,
		args ReviewArgs,
	) (raw string, err error)
}

// ReviewArgs is the provider-independent input. `ReviewPrompt` is
// always the full §1-§9 prompt assembled by buildReviewPrompt().
// `RepoRoot` is the workspace root (SWE-bench's <ws>/repo) so
// subprocess-based providers can mount it with --add-dir.
type ReviewArgs struct {
	ReviewPrompt string
	Issue        string
	Patch        string
	RepoRoot     string
	DomainTier   string
}
