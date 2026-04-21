// Package config — curated list of main-agent LLM presets.
//
// Presets are the user-facing menu in /model and /settings. Each entry
// bundles the three knobs banya-cli feeds to banya-core:
//   - endpoint (URL)
//   - model id
//   - API key source (env var that supplies it)
// plus an X-Target-Port header for routing inside the llm-server proxy.
//
// Keep this list short. Three to five presets cover 90% of user intent;
// advanced users can still edit ~/.config/banya/config.yaml by hand.
package config

import "os"

// LLMPreset describes one selectable main-model option.
type LLMPreset struct {
	// ID is the stable handle the user types: `/model qwen`, `/model gemini`.
	ID string
	// Label is shown in menus. Human-readable.
	Label string
	// Description is a short sentence explaining the trade-off.
	Description string
	// URL is the OpenAI-compatible endpoint banya-core talks to.
	// Empty for non-HTTP backends (e.g. claude-cli spawns a subprocess).
	URL string
	// Model is the model id passed as `model` in chat/completions bodies.
	Model string
	// TargetPort routes to a specific backend behind the llm-server proxy
	// (empty for direct-to-provider endpoints like Google / Anthropic).
	TargetPort string
	// APIKeyEnv names the env var that should supply the API key. We
	// don't bake keys into the preset — the user must export it.
	// Empty when the backend doesn't need an API key (CLI subprocess
	// backends authenticate via their own mechanism, e.g. `claude login`
	// OAuth for claude-cli).
	APIKeyEnv string
	// BackendID selects the registry factory (internal/client/backends_init.go).
	// Empty = use the historical llm-server HTTP client. Set to "claude-cli",
	// "gemini", "gemini-native", etc. to route through a different adapter.
	// When non-empty AND URL is empty, the backend is treated as a subprocess
	// provider: the API-key gate in ApplyLLMPreset is skipped.
	BackendID string
	// Beta marks presets whose transport is experimental (e.g. Anthropic's
	// OpenAI-compat endpoint). The UI should show a hint.
	Beta bool
}

// LLMPresets is the ordered, canonical list.
var LLMPresets = []LLMPreset{
	{
		ID:          "qwen",
		Label:       "Banya-Qwen3.5-122b (default)",
		Description: "Internal llm-server (Qwen 3.5 122B on vLLM). No external cost.",
		URL:         "http://118.37.145.31:5174",
		Model:       "Qwen3.5-122B",
		TargetPort:  "8085",
		APIKeyEnv:   "LLM_SERVER_API_KEY",
	},
	{
		ID:          "gemini",
		Label:       "Gemini 3 Flash Preview",
		Description: "Google Gemini via OpenAI-compat. Fast, low latency; needs GEMINI_KEY.",
		URL:         "https://generativelanguage.googleapis.com/v1beta/openai",
		Model:       "gemini-3-flash-preview",
		APIKeyEnv:   "GEMINI_KEY",
	},
	{
		ID:          "claude-opus",
		Label:       "Claude Opus 4.7 (Claude Code)",
		Description: "Anthropic Claude Opus 4.7 via OpenAI-compat beta endpoint. Needs ANTHROPIC_API_KEY (Claude MAX plan covers this).",
		URL:         "https://api.anthropic.com/v1",
		Model:       "claude-opus-4-7",
		APIKeyEnv:   "ANTHROPIC_API_KEY",
		Beta:        true,
	},
	{
		ID:          "claude-cli",
		Label:       "Claude Opus via Claude CLI (MAX plan, no API key)",
		Description: "Spawns the local `claude -p` subprocess. Uses Claude MAX OAuth (run `claude login` once). No ANTHROPIC_API_KEY required; zero per-token billing.",
		BackendID:   "claude-cli",
		Model:       "opus",
		// No URL, no APIKeyEnv — subprocess backend.
	},
}

// LookupPreset returns the preset with the given ID (or nil).
func LookupPreset(id string) *LLMPreset {
	for i := range LLMPresets {
		if LLMPresets[i].ID == id {
			return &LLMPresets[i]
		}
	}
	return nil
}

// MatchPresetFromConfig returns the preset whose URL+Model match the
// current config, or nil if the user's config is off-menu (hand-edited).
func MatchPresetFromConfig(c LLMServerConfig) *LLMPreset {
	for i := range LLMPresets {
		p := &LLMPresets[i]
		if p.URL == c.URL && p.Model == c.Model {
			return p
		}
	}
	return nil
}

// Resolve expands the preset into a concrete LLMServerConfig, pulling
// the API key from the named env var. An empty key is returned without
// error — callers should warn the user to export the variable.
func (p LLMPreset) Resolve() LLMServerConfig {
	return LLMServerConfig{
		URL:        p.URL,
		APIKey:     os.Getenv(p.APIKeyEnv),
		Model:      p.Model,
		TargetPort: p.TargetPort,
	}
}
