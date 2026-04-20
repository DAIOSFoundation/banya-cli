// Register the built-in backend adapters in the shared factory
// registry. Each init() picks the best existing constructor and wraps
// it; future adapters just add their own file with its own init().
//
// Defaults in this file are the same ones historically hard-coded in
// cmd/banya/run.go's switch statement — consolidating them here means
// a new adapter only has to register, not teach the switch.
package client

import "strings"

func init() {
	// "llm-server" — OpenAI-compat HTTP client against vLLM / LLM Lab.
	// Historical default. If the caller left everything empty, fall
	// back to whatever flags run.go feeds in (llm_server.url /
	// llm_server.model). Those flag-derived values get packed into
	// BackendConfig by run.go before calling NewBackendFromConfig.
	Register("llm-server", func(cfg BackendConfig) (LLMBackend, error) {
		return NewLLMServerClientWithTarget(
			cfg.Endpoint, cfg.APIKey, cfg.Model, cfg.TargetPort,
		), nil
	})

	// "gemini" — OpenAI-compat path against Google's /v1beta/openai
	// endpoint. Native tool-calling works because it's OpenAI-shaped;
	// this is the recommended Gemini path.
	Register("gemini", func(cfg BackendConfig) (LLMBackend, error) {
		endpoint := firstNonEmpty(
			cfg.Endpoint,
			"https://generativelanguage.googleapis.com/v1beta/openai",
		)
		model := firstNonEmpty(cfg.Model, "gemini-3-flash-preview")
		// X-Target-Port is vLLM-specific; Gemini ignores it.
		return NewLLMServerClientWithTarget(endpoint, cfg.APIKey, model, ""), nil
	})

	// "gemini-native" — experimental REST `generateContent` path with
	// Gemini's native functionCall format. Kept for ablation runs.
	Register("gemini-native", func(cfg BackendConfig) (LLMBackend, error) {
		endpoint := firstNonEmpty(cfg.Endpoint, DefaultGeminiEndpoint)
		model := firstNonEmpty(cfg.Model, DefaultGeminiModel)
		return NewGeminiBackend(endpoint, cfg.APIKey, model), nil
	})

	// Back-compat aliases so prior env values still resolve.
	Register("vllm", registry["llm-server"])
	Register("google", registry["gemini"])
	Register("anthropic-cli", registry["claude-cli"])
	Register("claude", registry["claude-cli"])
}

// providerAliasCanonical resolves human-friendly aliases to their
// registry key. Exposed for UI layers (/config, /model) that want to
// display a stable name even when the user set an alias.
func providerAliasCanonical(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "vllm":
		return "llm-server"
	case "google":
		return "gemini"
	case "anthropic-cli", "claude":
		return "claude-cli"
	default:
		return key
	}
}
