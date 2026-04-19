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
	Mode       string          `mapstructure:"mode"` // "sidecar" (default), "remote"
	PromptMode string          `mapstructure:"prompt_mode"` // code | ask | plan | agent (default: agent)
	Language   string          `mapstructure:"language"` // ko | en — default UI language for agent responses (user input language still wins per-turn)
	Sidecar    SidecarConfig   `mapstructure:"sidecar"`
	Server     ServerConfig    `mapstructure:"server"`
	LLMServer  LLMServerConfig `mapstructure:"llm_server"`
	Subagent   SubagentConfig  `mapstructure:"subagent"` // optional secondary model (critic, compactor 등)
	UI         UIConfig        `mapstructure:"ui"`
	Shell      ShellConfig     `mapstructure:"shell"`
	Log        LogConfig       `mapstructure:"log"`
}

// Supported language codes. Keep in lock-step with banya-core's
// PromptComposer.languageRule switch.
const (
	LanguageKorean  = "ko"
	LanguageEnglish = "en"
)

// NormalizeLanguage canonicalises free-form language strings ("Korean",
// "한국어", "KO") to the two-letter code used throughout the config /
// env / core layers. Returns "" if nothing recognisable — callers should
// treat that as "leave default".
func NormalizeLanguage(s string) string {
	switch s {
	case "", "ko", "KO", "Korean", "korean", "한국어":
		if s == "" {
			return ""
		}
		return LanguageKorean
	case "en", "EN", "English", "english":
		return LanguageEnglish
	}
	return ""
}

// SubagentConfig configures a secondary LLM used for sub-agent tasks
// (critic review, compaction, intent detection, etc.). When set, the
// CLI propagates these values to banya-core as SUBAGENT_* env vars on
// sidecar spawn; banya-core bootstraps an AdminModelConfig and makes
// it available to spawn_agent / compactor / intent routing.
type SubagentConfig struct {
	Provider string `mapstructure:"provider"`  // gemini | anthropic | openai-compat (empty = disabled)
	Model    string `mapstructure:"model"`     // e.g. gemini-3-flash-preview
	APIKey   string `mapstructure:"api_key"`
	Endpoint string `mapstructure:"endpoint"` // optional override; provider-default used if empty
}

// SidecarConfig controls how the CLI locates and runs the banya-core sidecar binary.
type SidecarConfig struct {
	// Path is an explicit path to the sidecar binary. Empty → auto-resolve.
	Path string `mapstructure:"path"`
}

// LLMServerConfig targets an OpenAI-compatible llm-server (LLM Lab).
// The cli forwards every llm.chat host call through this backend.
type LLMServerConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`
	Model  string `mapstructure:"model"`
	// TargetPort is the X-Target-Port header value used by the LLM
	// Lab Client Manager to route to a specific vLLM/SGLang instance
	// behind the proxy (e.g. "8085" → Qwen3.5-122B vLLM).
	TargetPort string `mapstructure:"target_port"`
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
	v.SetDefault("mode", "sidecar")
	v.SetDefault("prompt_mode", "agent")
	v.SetDefault("language", LanguageKorean)
	v.SetDefault("sidecar.path", "")
	v.SetDefault("server.url", "http://localhost:8080")
	v.SetDefault("server.api_key", "")
	v.SetDefault("llm_server.url", "http://118.37.145.31:5174")
	v.SetDefault("llm_server.api_key", "sk-959b0eb4a8899f7e194f294eeebde0235956425ba77c56de")
	v.SetDefault("llm_server.model", "/models/model")
	v.SetDefault("llm_server.target_port", "8085")
	v.SetDefault("subagent.provider", "")
	v.SetDefault("subagent.model", "")
	v.SetDefault("subagent.api_key", "")
	v.SetDefault("subagent.endpoint", "")
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

// SaveLanguage persists only the `language` key, preserving every other
// section. Used by the /language slash command for fast single-field edits.
func SaveLanguage(lang string) error {
	if err := EnsureDirs(); err != nil {
		return err
	}
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(configDir())
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return fmt.Errorf("read config: %w", err)
		}
	}
	v.Set("language", lang)
	out := filepath.Join(configDir(), "config.yaml")
	if err := v.WriteConfigAs(out); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// SaveSubagent persists the subagent config to the on-disk config file.
// Other config sections are preserved (viper re-reads the file first).
// Called by the /settings TUI after the user submits the form.
func SaveSubagent(cfg SubagentConfig) error {
	if err := EnsureDirs(); err != nil {
		return err
	}
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(configDir())
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return fmt.Errorf("read config: %w", err)
		}
	}
	v.Set("subagent.provider", cfg.Provider)
	v.Set("subagent.model", cfg.Model)
	v.Set("subagent.api_key", cfg.APIKey)
	v.Set("subagent.endpoint", cfg.Endpoint)
	out := filepath.Join(configDir(), "config.yaml")
	if err := v.WriteConfigAs(out); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
