package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// keysFilePath returns the path to the stored LLM API keys file. Kept
// separate from auth.json (remote-server bearer token) and config.yaml
// (user preferences) so the on-disk permission model can be stricter
// — 0600, owner-only — without affecting the parts users routinely
// open in their editor.
func keysFilePath() string {
	return filepath.Join(configDir(), "keys.json")
}

// LLMKeys is a map of preset ID → API key value. The preset ID
// matches LLMPreset.ID ("qwen", "gemini", "claude-opus", ...); the
// claude-cli preset has no APIKeyEnv so it never gets stored here.
type LLMKeys map[string]string

// LoadLLMKeys reads the key map from disk. Returns an empty map (not
// nil) when the file doesn't exist so callers can treat missing
// entries as "not set" uniformly. Silent on any read error beyond
// "file missing" — a corrupt keys.json shouldn't break banya's
// startup; the user just sees no keys resolved.
func LoadLLMKeys() LLMKeys {
	data, err := os.ReadFile(keysFilePath())
	if err != nil {
		return LLMKeys{}
	}
	var keys LLMKeys
	if err := json.Unmarshal(data, &keys); err != nil {
		return LLMKeys{}
	}
	if keys == nil {
		keys = LLMKeys{}
	}
	return keys
}

// SaveLLMKey persists `key` under the given preset ID. Pass an empty
// key to remove the entry. Writes 0600 so only the owning user can
// read the file.
func SaveLLMKey(presetID, key string) error {
	if presetID == "" {
		return fmt.Errorf("preset id is required")
	}
	keys := LoadLLMKeys()
	if key == "" {
		delete(keys, presetID)
	} else {
		keys[presetID] = key
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(keysFilePath(), data, 0o600); err != nil {
		return fmt.Errorf("write keys: %w", err)
	}
	return nil
}

// ResolveLLMKey returns the API key for the given preset, trying in
// priority order:
//   1. process env (named by preset.APIKeyEnv), so shell exports still
//      win over the stored file — matches established 12-factor habit
//      and lets CI/test runs shadow the saved value.
//   2. keys.json (persisted by `/key <value>` inside the TUI).
//   3. empty string, caller decides whether that's acceptable.
func ResolveLLMKey(preset *LLMPreset) string {
	if preset == nil {
		return ""
	}
	if preset.APIKeyEnv != "" {
		if v := os.Getenv(preset.APIKeyEnv); v != "" {
			return v
		}
	}
	keys := LoadLLMKeys()
	return keys[preset.ID]
}
