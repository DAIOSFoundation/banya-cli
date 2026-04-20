// Backend registry — one place to enumerate and construct every
// LLMBackend the CLI knows about. Each provider (llm-server / gemini /
// gemini-native / claude-cli / …) registers a factory function; callers
// look up by name and receive a ready-to-use LLMBackend.
//
// Why a registry instead of run.go's big switch? Three reasons:
//   1. Add a new adapter = add one file + one line. No more editing
//      run.go to thread flags through.
//   2. Uniform config resolution. BackendConfig carries the union of
//      knobs every backend might read; ResolveBackendConfig() pulls
//      them from env + CLI flags with a single well-known precedence.
//   3. Tests can swap the registry for a mock without patching run.go.
//
// Invariant: every factory MUST be callable with a fully-defaulted
// BackendConfig (empty strings) and return a usable backend whose
// Chat() fails cleanly if required fields (e.g. API key) are missing.
// No panics on bad config.
package client

import (
	"fmt"
	"os"
	"strings"
)

// BackendConfig is the union of knobs any adapter might read. Each
// provider picks the subset it cares about; unknown fields are
// ignored silently.
type BackendConfig struct {
	// Provider is the registry key. Known values:
	//   "llm-server"      (default) OpenAI-compat HTTP client pointed
	//                     at vLLM / our on-prem Qwen proxy.
	//   "gemini"          OpenAI-compat against Google's /v1beta/openai
	//                     endpoint. Same client as llm-server; just a
	//                     different URL + model default.
	//   "gemini-native"   Gemini REST functionCall path (experimental).
	//   "claude-cli"      Spawn local `claude -p` subprocess per turn;
	//                     matches Claude MAX plan, zero API cost.
	Provider string

	// Endpoint is the HTTP base URL (for HTTP backends) or ignored
	// (for subprocess backends).
	Endpoint string

	// APIKey is the bearer / query-param credential. Resolution in
	// ResolveBackendConfig tries BANYA_MAIN_API_KEY first, then
	// provider-specific env vars, then the --llm-key CLI flag.
	APIKey string

	// Model is the model id. Resolution tries BANYA_MAIN_MODEL first,
	// then the provider's default.
	Model string

	// TargetPort is the X-Target-Port header (LLM Lab vLLM routing).
	// Only the llm-server provider consumes it.
	TargetPort string

	// BinaryPath is the path to a subprocess executable (claude-cli).
	// Empty = resolve via PATH.
	BinaryPath string
}

// BackendFactory builds an LLMBackend from config. Returning an error
// is reserved for malformed config (unknown provider, required field
// missing BEFORE we even start); missing credentials that only matter
// at call-time should be left for Chat() to surface.
type BackendFactory func(cfg BackendConfig) (LLMBackend, error)

// registry maps provider → factory. Populated by each adapter file's
// init() or by explicit Register() calls. Keep the list short —
// every entry here is a supported deployment path.
var registry = map[string]BackendFactory{}

// Register adds a backend factory under a provider name. Last write
// wins if the same name is registered twice, which makes tests able
// to swap in a mock without touching production code.
func Register(name string, factory BackendFactory) {
	registry[strings.ToLower(name)] = factory
}

// KnownProviders returns the sorted list of registered provider names
// — useful for error messages and `banya --help` output.
func KnownProviders() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	// Stable order for error messages.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// NewBackendFromConfig picks a factory by cfg.Provider and invokes it.
// The zero-value provider ("") is treated as "llm-server" — the
// historical default.
func NewBackendFromConfig(cfg BackendConfig) (LLMBackend, error) {
	key := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if key == "" {
		key = "llm-server"
	}
	factory, ok := registry[key]
	if !ok {
		return nil, fmt.Errorf(
			"unknown backend provider %q (known: %v)", cfg.Provider, KnownProviders(),
		)
	}
	return factory(cfg)
}

// ResolveBackendConfig pulls every relevant knob from the environment
// (BANYA_MAIN_*, GEMINI_KEY, ANTHROPIC_API_KEY, BANYA_CLAUDE_CLI_BIN)
// and returns a BackendConfig. Flag overrides are layered on by the
// caller in run.go because flags are scoped to a cobra.Command.
//
// Precedence for apiKey: BANYA_MAIN_API_KEY > provider-specific env >
// empty (factory will error out at call-time if required).
func ResolveBackendConfig() BackendConfig {
	provider := firstNonEmpty(
		os.Getenv("BANYA_MAIN_PROVIDER"),
		// Historical default. Matches "unset" semantics of the old
		// switch statement.
		"llm-server",
	)
	apiKey := firstNonEmpty(
		os.Getenv("BANYA_MAIN_API_KEY"),
		// Provider-specific fallbacks. Harmless to include all three
		// since only the matching one applies.
		os.Getenv("GEMINI_KEY"),
		os.Getenv("ANTHROPIC_API_KEY"),
	)
	return BackendConfig{
		Provider:   provider,
		Endpoint:   os.Getenv("BANYA_MAIN_ENDPOINT"),
		APIKey:     apiKey,
		Model:      os.Getenv("BANYA_MAIN_MODEL"),
		TargetPort: os.Getenv("BANYA_MAIN_TARGET_PORT"),
		BinaryPath: os.Getenv("BANYA_CLAUDE_CLI_BIN"),
	}
}

// firstNonEmpty returns the first argument that isn't the empty string.
// Defined here (not llmserver.go) so adapters can share it without
// reaching across files.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
