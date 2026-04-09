package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AuthToken holds the API authentication token.
type AuthToken struct {
	Token     string `json:"token"`
	ServerURL string `json:"server_url"`
}

// authFilePath returns the path to the stored auth token.
func authFilePath() string {
	return filepath.Join(configDir(), "auth.json")
}

// SaveToken persists the API token to disk.
func SaveToken(token AuthToken) error {
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(authFilePath(), data, 0o600); err != nil {
		return fmt.Errorf("write token: %w", err)
	}
	return nil
}

// LoadToken reads the stored API token from disk.
func LoadToken() (*AuthToken, error) {
	data, err := os.ReadFile(authFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read token: %w", err)
	}
	var token AuthToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("unmarshal token: %w", err)
	}
	return &token, nil
}

// ClearToken removes the stored API token.
func ClearToken() error {
	err := os.Remove(authFilePath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove token: %w", err)
	}
	return nil
}
