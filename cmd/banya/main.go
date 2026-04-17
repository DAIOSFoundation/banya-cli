package main

import (
	"fmt"
	"os"

	"github.com/cascadecodes/banya-cli/internal/app"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/internal/setup"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func main() {
	rootCmd := &cobra.Command{
		Use:   "banya",
		Short: "Banya - AI code agent CLI",
		Long:  "Banya is a terminal-based AI code agent that connects to a server-side code agent API.",
		RunE:  runChat,
	}

	// Global flags
	rootCmd.PersistentFlags().String("mode", "", "Client mode: sidecar (default) | remote")
	rootCmd.PersistentFlags().String("sidecar", "", "Path to banya-core sidecar binary (sidecar mode only)")
	rootCmd.PersistentFlags().String("llm-url", "", "llm-server base URL (llm-server/sidecar modes)")
	rootCmd.PersistentFlags().String("llm-key", "", "llm-server API key (llm-server/sidecar modes)")
	rootCmd.PersistentFlags().String("llm-model", "", "llm-server model id (llm-server/sidecar modes)")
	rootCmd.PersistentFlags().StringP("server", "s", "", "Remote banya-core URL (remote mode only)")
	rootCmd.PersistentFlags().StringP("api-key", "k", "", "API key for remote banya-core")
	rootCmd.PersistentFlags().String("theme", "", "UI theme: dark, light")

	// Subcommands
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(loginCmd())
	rootCmd.AddCommand(logoutCmd())
	rootCmd.AddCommand(configCmd())
	rootCmd.AddCommand(historyCmd())
	rootCmd.AddCommand(setupCmd())
	rootCmd.AddCommand(serveCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// runChat is the default command that starts the interactive TUI.
func runChat(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Apply CLI flag overrides
	if m, _ := cmd.Flags().GetString("mode"); m != "" {
		cfg.Mode = m
	}
	if sc, _ := cmd.Flags().GetString("sidecar"); sc != "" {
		cfg.Sidecar.Path = sc
	}
	if u, _ := cmd.Flags().GetString("llm-url"); u != "" {
		cfg.LLMServer.URL = u
	}
	if k, _ := cmd.Flags().GetString("llm-key"); k != "" {
		cfg.LLMServer.APIKey = k
	}
	if m, _ := cmd.Flags().GetString("llm-model"); m != "" {
		cfg.LLMServer.Model = m
	}
	if s, _ := cmd.Flags().GetString("server"); s != "" {
		cfg.Server.URL = s
	}
	if k, _ := cmd.Flags().GetString("api-key"); k != "" {
		cfg.Server.APIKey = k
	}
	if t, _ := cmd.Flags().GetString("theme"); t != "" {
		cfg.UI.Theme = t
	}

	return app.Run(cfg)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("banya version %s\n", version)
		},
	}
}

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the code agent server",
		RunE: func(cmd *cobra.Command, args []string) error {
			var serverURL, apiKey string
			fmt.Print("Server URL (default: http://localhost:8080): ")
			fmt.Scanln(&serverURL)
			if serverURL == "" {
				serverURL = "http://localhost:8080"
			}

			fmt.Print("API Key: ")
			fmt.Scanln(&apiKey)
			if apiKey == "" {
				return fmt.Errorf("API key is required")
			}

			token := config.AuthToken{
				Token:     apiKey,
				ServerURL: serverURL,
			}
			if err := config.SaveToken(token); err != nil {
				return err
			}

			fmt.Println("Login successful. Credentials saved.")
			return nil
		},
	}
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.ClearToken(); err != nil {
				return err
			}
			fmt.Println("Credentials removed.")
			return nil
		},
	}
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show configuration",
		Run: func(cmd *cobra.Command, args []string) {
			cfg, err := config.Load()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return
			}
			fmt.Printf("Config file:    %s\n", config.ConfigFilePath())
			fmt.Printf("Mode:           %s\n", cfg.Mode)
			fmt.Printf("Sidecar path:   %s\n", cfg.Sidecar.Path)
			fmt.Printf("LLM server URL: %s\n", cfg.LLMServer.URL)
			fmt.Printf("LLM model:      %s\n", cfg.LLMServer.Model)
			fmt.Printf("Remote URL:     %s\n", cfg.Server.URL)
			fmt.Printf("Theme:          %s\n", cfg.UI.Theme)
			fmt.Printf("Shell:          %s\n", cfg.Shell.Shell)
			fmt.Printf("Log level:      %s\n", cfg.Log.Level)
		},
	}
	return cmd
}

func historyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "List recent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: integrate with session.History
			fmt.Println("No session history yet.")
			return nil
		},
	}
}

func setupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up Banya CLI (install Nerd Font, configure terminal)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Banya CLI Setup")
			fmt.Println("================")
			fmt.Println()

			// 1. Font check
			if setup.HasNerdFont() {
				fmt.Println("  ✓ Nerd Font: installed")
			} else {
				fmt.Println("  ✗ Nerd Font: not found")
				fmt.Printf("  Installing %s...\n", setup.DefaultFont.Name)
				err := setup.InstallNerdFont(setup.DefaultFont, func(msg string) {
					fmt.Printf("    → %s\n", msg)
				})
				if err != nil {
					fmt.Printf("    ✗ Failed: %v\n", err)
					fmt.Println("    Manual install: https://www.nerdfonts.com/")
				} else {
					fmt.Println("  ✓ Nerd Font: installed")
					setup.SetGnomeTerminalFont("JetBrainsMono Nerd Font Mono", 12)
					fmt.Println("  ✓ Terminal font: configured")
				}
			}
			fmt.Println()

			// 2. Config check
			if err := config.EnsureDirs(); err != nil {
				fmt.Printf("  ✗ Config dirs: %v\n", err)
			} else {
				fmt.Printf("  ✓ Config: %s\n", config.ConfigFilePath())
				fmt.Printf("  ✓ Data:   %s\n", config.DataDir())
			}
			fmt.Println()

			// 3. Token check
			token, _ := config.LoadToken()
			if token != nil {
				fmt.Printf("  ✓ Auth: logged in (%s)\n", token.ServerURL)
			} else {
				fmt.Println("  ○ Auth: not logged in (run 'banya login')")
			}
			fmt.Println()

			fmt.Println("Setup complete. Run 'banya' to start.")
			return nil
		},
	}
	return cmd
}
