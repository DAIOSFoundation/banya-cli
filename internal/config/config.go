// Package config manages application configuration using Viper.
// Config is loaded from (in order of precedence):
//  1. CLI flags
//  2. Environment variables (BANYA_ prefix)
//  3. Config file (~/.config/banya/config.yaml)
//  4. Defaults
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config holds the complete application configuration.
type Config struct {
	Server ServerConfig `mapstructure:"server"`
	UI     UIConfig     `mapstructure:"ui"`
	Shell  ShellConfig  `mapstructure:"shell"`
	Log    LogConfig    `mapstructure:"log"`
}

// ServerConfig holds settings for connecting to the code agent API.
type ServerConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`
}

// UIConfig holds terminal UI preferences.
type UIConfig struct {
	Theme      string `mapstructure:"theme"`       // dark, light
	ShowTokens bool   `mapstructure:"show_tokens"`  // display token count
	WordWrap   bool   `mapstructure:"word_wrap"`
	MaxWidth   int    `mapstructure:"max_width"`    // 0 = auto (terminal width)
}

// ShellConfig controls how the CLI handles server-requested shell commands.
type ShellConfig struct {
	AutoApprove    bool     `mapstructure:"auto_approve"`     // skip approval for low-risk commands
	AllowedCommands []string `mapstructure:"allowed_commands"` // commands that never need approval
	BlockedCommands []string `mapstructure:"blocked_commands"` // commands that are always denied
	Shell          string   `mapstructure:"shell"`            // shell to use (default: $SHELL or /bin/bash)
}

// LogConfig controls logging behavior.
type LogConfig struct {
	Level string `mapstructure:"level"` // debug, info, warn, error
	File  string `mapstructure:"file"`  // log file path, empty = stderr only
}

// configDir returns the banya config directory path.
func configDir() string {
	if dir := os.Getenv("BANYA_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "banya")
}

// DataDir returns the banya data directory path (for sessions, history).
func DataDir() string {
	if dir := os.Getenv("BANYA_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "banya")
}

// Load reads configuration from all sources and returns a Config struct.
func Load() (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("server.url", "http://localhost:8080")
	v.SetDefault("server.api_key", "")
	v.SetDefault("ui.theme", "dark")
	v.SetDefault("ui.show_tokens", true)
	v.SetDefault("ui.word_wrap", true)
	v.SetDefault("ui.max_width", 0)
	v.SetDefault("shell.auto_approve", false)
	v.SetDefault("shell.allowed_commands", []string{"ls", "cat", "head", "tail", "wc", "pwd", "whoami", "date", "echo"})
	v.SetDefault("shell.blocked_commands", []string{"rm -rf /", "mkfs", "dd if=/dev/zero", ":(){:|:&};:"})
	v.SetDefault("shell.shell", "")
	v.SetDefault("log.level", "info")
	v.SetDefault("log.file", "")

	// Config file
	cfgDir := configDir()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(cfgDir)
	v.AddConfigPath(".")

	// Environment variables: BANYA_SERVER_URL, BANYA_SERVER_API_KEY, etc.
	v.SetEnvPrefix("BANYA")
	v.AutomaticEnv()

	// Read config file (ignore if not found)
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Resolve shell if not set
	if cfg.Shell.Shell == "" {
		cfg.Shell.Shell = os.Getenv("SHELL")
		if cfg.Shell.Shell == "" {
			cfg.Shell.Shell = "/bin/bash"
		}
	}

	return &cfg, nil
}

// EnsureDirs creates the config and data directories if they don't exist.
func EnsureDirs() error {
	for _, dir := range []string{configDir(), DataDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

// ConfigFilePath returns the expected path of the config file.
func ConfigFilePath() string {
	return filepath.Join(configDir(), "config.yaml")
}
