// Package app wires together the application components and starts the TUI.
package app

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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

	model := ui.New(apiClient, cfg)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("run TUI: %w", err)
	}

	return nil
}

// buildClient selects ProcessClient (default, spawns core sidecar) or
// HTTPClient when cfg.Sidecar.Remote is true.
func buildClient(cfg *config.Config, apiKey string) (client.Client, error) {
	if cfg.Sidecar.Remote {
		return client.NewHTTPClient(cfg.Server.URL, apiKey), nil
	}
	pc, err := client.NewProcessClient(cfg.Sidecar.Path)
	if err != nil {
		return nil, err
	}
	return pc, nil
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
