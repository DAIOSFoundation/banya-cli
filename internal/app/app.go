// Package app wires together the application components and starts the TUI.
package app

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cascadecodes/banya-cli/internal/audio"
	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/internal/setup"
	"github.com/cascadecodes/banya-cli/internal/ui"
)

// ANSI OSC sequences to override terminal colors.
const (
	setBlackBg  = "\033]11;#000000\a"
	setGreenFg  = "\033]10;#00FF41\a"
	resetColors = "\033]10;\a\033]11;\a"
)

// Run initializes and starts the banya TUI application.
func Run(cfg *config.Config) error {
	if err := config.EnsureDirs(); err != nil {
		return fmt.Errorf("create dirs: %w", err)
	}

	// Check Nerd Font on first run
	ensureNerdFont()

	// Resolve API key
	apiKey := cfg.Server.APIKey
	if apiKey == "" {
		token, err := config.LoadToken()
		if err != nil {
			return fmt.Errorf("load token: %w", err)
		}
		if token != nil {
			apiKey = token.Token
			if cfg.Server.URL == "http://localhost:8080" && token.ServerURL != "" {
				cfg.Server.URL = token.ServerURL
			}
		}
	}

	apiClient, err := buildClient(cfg, apiKey)
	if err != nil {
		return fmt.Errorf("init client: %w", err)
	}
	defer apiClient.Close()

	fmt.Fprint(os.Stdout, setBlackBg+setGreenFg)
	defer fmt.Fprint(os.Stdout, resetColors)

	// Start the Buddha banner chime asynchronously. Missing audio
	// player or errors are silent — never blocks TUI startup.
	audio.PlayStart()

	model := ui.New(apiClient, cfg)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("run TUI: %w", err)
	}

	return nil
}

// buildClient picks the Client implementation based on cfg.Mode.
//
//   - "remote"  → HTTPClient against cfg.Server.URL (a remote banya-core)
//   - "sidecar" → spawn local banya-core and host the LLM backend for it
func buildClient(cfg *config.Config, apiKey string) (client.Client, error) {
	switch cfg.Mode {
	case "remote":
		return client.NewHTTPClient(cfg.Server.URL, apiKey), nil
	case "", "sidecar":
		backend := client.NewLLMServerClientWithTarget(cfg.LLMServer.URL, cfg.LLMServer.APIKey, cfg.LLMServer.Model, cfg.LLMServer.TargetPort)
		pc, err := client.NewProcessClient(cfg.Sidecar.Path)
		if err != nil {
			return nil, err
		}
		pc.SetLLMBackend(backend)
		// Sidecar stderr would otherwise tear the Bubble Tea screen — it's
		// the path banya-core uses for console.log, Bun SEA boot probes,
		// LLM manager traces, etc. Route to a dated log file under the
		// user-scoped data dir, AND redirect our own os.Stderr FD there so
		// any `fmt.Fprintf(os.Stderr, ...)` in banya-cli (e.g. the
		// "[banya-cli] unparseable sidecar line" warning) lands in the same
		// file instead of tearing the TUI. BANYA_SIDECAR_STDERR=inherit
		// restores the legacy behaviour (both streams flow to the terminal).
		if os.Getenv("BANYA_SIDECAR_STDERR") != "inherit" {
			if w := openSidecarLog(); w != nil {
				pc.SetStderrSink(w)
				redirectProcessStderrToFile(w)
			}
		}
		// Propagate Subagent config (critic 모델 등) through env so
		// banya-core 가 자동 bootstrap. /settings 로 값이 바뀌면 현재
		// 프로세스는 그대로, 다음 CLI 시작 시 적용.
		env := client.SubagentEnvVars(cfg.Subagent.Provider, cfg.Subagent.Model, cfg.Subagent.APIKey, cfg.Subagent.Endpoint)
		// TUI opts into interactive runtime mode — banya-core's
		// ConversationManager skips QueryComposer (whose output would
		// otherwise stream onto the chat screen) and ContextGatherer
		// skips RepoMap (saves multi-second latency in large cwds).
		// `banya run` (headless path) leaves this env unset so codegen
		// and benchmark harnesses keep the full context pipeline.
		// Caller-supplied BANYA_RUNTIME_MODE wins so power users can
		// force headless behaviour in the TUI for debugging.
		if os.Getenv("BANYA_RUNTIME_MODE") == "" {
			env = append(env, "BANYA_RUNTIME_MODE=interactive")
		}
		if v := client.LanguageEnvVar(cfg.Language); v != "" {
			env = append(env, v)
		}
		if len(env) > 0 {
			pc.SetExtraEnv(env)
		}
		return pc, nil
	default:
		return nil, fmt.Errorf("unknown mode %q (want sidecar|remote)", cfg.Mode)
	}
}

// redirectProcessStderrToFile reassigns os.Stderr to the provided file
// so any `fmt.Fprintf(os.Stderr, ...)` call in banya-cli (e.g. the
// "[banya-cli] unparseable sidecar line" warning in process.go) lands
// in the log file instead of tearing the Bubble Tea screen.
//
// We reassign the Go-level variable rather than dup'ing fd 2 at the
// syscall layer because (1) the former is portable across darwin/linux/
// windows without build-tag gymnastics, and (2) it covers every call
// site that reads os.Stderr at invocation time. Runtime-level writes
// to fd 2 (Go panic traces, C library diagnostics) still hit the
// terminal, which is the right behaviour — a real panic should not be
// hidden in a log file the user has to discover.
//
// Silent on non-*os.File writers (shouldn't happen today; guard for
// future callers).
func redirectProcessStderrToFile(w io.Writer) {
	if f, ok := w.(*os.File); ok {
		os.Stderr = f
	}
}

// openSidecarLog returns a writer pointed at
// $XDG_DATA_HOME/banya/logs/sidecar-YYYYMMDD-HHMMSS.log (or
// ~/.local/share/banya/logs/… when XDG is unset). Returns nil on any
// failure — the caller falls back to os.Stderr in that case so a
// misconfigured filesystem can't break the TUI. Intentionally
// best-effort: no rotation / retention — the file is overwritten each
// invocation, which is fine because sidecar stderr is diagnostic,
// not audit material.
func openSidecarLog() io.Writer {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, ".local", "share")
	}
	logDir := filepath.Join(dir, "banya", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil
	}
	name := fmt.Sprintf("sidecar-%s.log", time.Now().Format("20060102-150405"))
	f, err := os.OpenFile(filepath.Join(logDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}


// ensureNerdFont checks for a Nerd Font and offers to install one if missing.
func ensureNerdFont() {
	if setup.HasNerdFont() {
		return
	}

	fmt.Println()
	fmt.Println("  ⚠  Nerd Font not detected.")
	fmt.Println("  Banya CLI uses Powerline glyphs that require a Nerd Font.")
	fmt.Println()
	fmt.Printf("  Install %s automatically? [Y/n] ", setup.DefaultFont.Name)

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "" && answer != "y" && answer != "yes" {
		fmt.Println("  Skipped. Some UI glyphs may not render correctly.")
		fmt.Println()
		return
	}

	err := setup.InstallNerdFont(setup.DefaultFont, func(msg string) {
		fmt.Printf("  → %s\n", msg)
	})
	if err != nil {
		fmt.Printf("  ✗ Font installation failed: %v\n", err)
		fmt.Println("  You can install manually: https://www.nerdfonts.com/")
		fmt.Println()
		return
	}

	// Apply to GNOME Terminal if available
	setup.SetGnomeTerminalFont("JetBrainsMono Nerd Font Mono", 12)
	fmt.Println("  ✓ Terminal font configured.")
	fmt.Println()
}
