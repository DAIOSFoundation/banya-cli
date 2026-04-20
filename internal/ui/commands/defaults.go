package commands

import (
	"fmt"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/google/uuid"
)

// OpenSettingsMsg is emitted by /settings and triggers the main UI
// model to switch into StateSettings. Lives in this package so the
// commands handler can return it as a tea.Cmd without importing
// internal/ui (which would cause a cycle).
type OpenSettingsMsg struct{}

func (r *Registry) registerDefaults() {
	r.Register(&Command{
		Name:    "help",
		Aliases: []string{"?"},
		Summary: "List available slash commands",
		Handler: r.helpHandler,
	})
	r.Register(&Command{
		Name:    "quit",
		Aliases: []string{"exit", "q"},
		Summary: "Exit banya",
		Handler: func(_ Context, _ []string) Result {
			return Result{Quit: true}
		},
	})
	r.Register(&Command{
		Name:    "clear",
		Aliases: []string{"cls"},
		Summary: "Clear the conversation",
		Handler: func(_ Context, _ []string) Result {
			return Result{Clear: true}
		},
	})
	r.Register(&Command{
		Name:    "new",
		Summary: "Start a fresh session (new session_id, clear history)",
		Handler: func(_ Context, _ []string) Result {
			return Result{
				Clear:  true,
				Output: "new session: " + uuid.New().String(),
			}
		},
	})
	r.Register(&Command{
		Name:    "config",
		Summary: "Print the current configuration",
		Handler: configHandler,
	})
	r.Register(&Command{
		Name:    "mode",
		Usage:   "/mode [code|ask|plan|agent]",
		Summary: "Switch (or show) the active prompt mode",
		Handler: promptModeHandler,
	})
	r.Register(&Command{
		Name:    "transport",
		Summary: "Print the active transport (sidecar / remote)",
		Handler: func(ctx Context, _ []string) Result {
			mode := ctx.Config.Mode
			if mode == "" {
				mode = "sidecar"
			}
			return Result{Output: "transport: " + mode}
		},
	})
	r.Register(&Command{
		Name:    "ping",
		Summary: "Health-check the active client (sidecar or remote core)",
		Handler: func(ctx Context, _ []string) Result {
			if err := ctx.Client.HealthCheck(); err != nil {
				return Result{Output: "ping failed: " + err.Error()}
			}
			return Result{Output: "ping ok"}
		},
	})
	r.Register(&Command{
		Name:    "sidecar",
		Summary: "Show banya-core sidecar info (path, platform)",
		Handler: sidecarHandler,
	})
	r.Register(&Command{
		Name:    "session",
		Usage:   "/session",
		Summary: "Print the current session id",
		Handler: func(ctx Context, _ []string) Result {
			return Result{Output: "session: " + ctx.SessionID}
		},
	})
	r.Register(&Command{
		Name:    "history",
		Summary: "Show local conversation history count (stored in memory)",
		Handler: func(_ Context, _ []string) Result {
			return Result{Output: "history is kept in-memory for this session; use /new to reset"}
		},
	})
	r.Register(&Command{
		Name:    "version",
		Summary: "Print client + protocol version",
		Handler: versionHandler,
	})
	r.Register(&Command{
		Name:    "settings",
		Usage:   "/settings",
		Summary: "Open the interactive settings screen (language, subagent/critic model)",
		Handler: func(_ Context, _ []string) Result {
			return Result{
				Cmd: func() tea.Msg { return OpenSettingsMsg{} },
			}
		},
	})
	r.Register(&Command{
		Name:    "language",
		Aliases: []string{"lang"},
		Usage:   "/language [ko|en]",
		Summary: "Show or set the default response language (ko | en). Per-turn input language still wins.",
		Handler: languageHandler,
	})
	r.Register(&Command{
		Name:    "model",
		Aliases: []string{"llm"},
		Usage:   "/model [id|index]",
		Summary: "Show or switch the main LLM preset (qwen | gemini | claude-opus)",
		Handler: modelHandler,
	})
}

func modelHandler(ctx Context, args []string) Result {
	current := config.MatchPresetFromConfig(ctx.Config.LLMServer)

	if len(args) == 0 {
		// List + mark current.
		var b strings.Builder
		b.WriteString("LLM presets (use `/model <id>` to switch):\n")
		for i, p := range config.LLMPresets {
			mark := "  "
			if current != nil && current.ID == p.ID {
				mark = "* "
			}
			beta := ""
			if p.Beta {
				beta = "  [beta]"
			}
			fmt.Fprintf(&b, "%s%d. %s (%s)%s\n    %s\n    env: %s\n",
				mark, i+1, p.Label, p.ID, beta, p.Description, p.APIKeyEnv)
		}
		if current == nil {
			b.WriteString("\n(current LLM config doesn't match any preset — hand-edited config.yaml)")
		}
		return Result{Output: strings.TrimRight(b.String(), "\n")}
	}

	// Accept either preset ID or 1-based index.
	arg := strings.ToLower(args[0])
	var target *config.LLMPreset
	if idx, err := parsePresetIndex(arg); err == nil && idx >= 0 && idx < len(config.LLMPresets) {
		target = &config.LLMPresets[idx]
	} else {
		target = config.LookupPreset(arg)
	}
	if target == nil {
		return Result{Output: "unknown preset: " + args[0] + "  (try /model to list)"}
	}
	if ctx.ApplyLLMPreset == nil {
		return Result{Output: "model switcher not wired in this client"}
	}
	if err := ctx.ApplyLLMPreset(target.ID); err != nil {
		return Result{Output: "failed to apply preset " + target.ID + ": " + err.Error()}
	}
	msg := "model → " + target.Label + "  (" + target.Model + ")"
	if target.Beta {
		msg += "  [beta — verify tool calls work]"
	}
	return Result{Output: msg}
}

func parsePresetIndex(s string) (int, error) {
	// 1-based → 0-based. Accept "1", "2", "3".
	switch s {
	case "1":
		return 0, nil
	case "2":
		return 1, nil
	case "3":
		return 2, nil
	case "4":
		return 3, nil
	case "5":
		return 4, nil
	}
	return -1, fmt.Errorf("not an index")
}

func languageHandler(ctx Context, args []string) Result {
	current := ctx.Config.Language
	if current == "" {
		current = config.LanguageKorean
	}
	if len(args) == 0 {
		return Result{Output: "language: " + current + "  (choices: ko | en)"}
	}
	next := config.NormalizeLanguage(args[0])
	if next == "" {
		return Result{Output: "unknown language: " + args[0] + "  (want ko | en)"}
	}
	if ctx.SetLanguage == nil {
		return Result{Output: "language setter not wired in this client"}
	}
	if err := ctx.SetLanguage(next); err != nil {
		return Result{Output: "failed to save language: " + err.Error()}
	}
	return Result{Output: "language → " + next + " (takes effect after `/new` or CLI restart)"}
}

func promptModeHandler(ctx Context, args []string) Result {
	current := protocol.PromptAsk
	if ctx.PromptMode != nil {
		current = ctx.PromptMode()
	}
	if len(args) == 0 {
		return Result{Output: "prompt mode: " + string(current) +
			"  (choices: code | ask | plan | agent)"}
	}
	next := protocol.PromptType(strings.ToLower(args[0]))
	switch next {
	case protocol.PromptCode, protocol.PromptAsk, protocol.PromptPlan, protocol.PromptAgent:
		if ctx.SetPromptMode == nil {
			return Result{Output: "prompt mode setter not wired in this client"}
		}
		if err := ctx.SetPromptMode(next); err != nil {
			return Result{Output: "failed to switch mode: " + err.Error()}
		}
		return Result{Output: "prompt mode → " + string(next)}
	default:
		return Result{Output: "unknown mode: " + args[0] +
			"  (want code | ask | plan | agent)"}
	}
}

func (r *Registry) helpHandler(_ Context, _ []string) Result {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, c := range r.All() {
		line := "  /" + c.Name
		if len(c.Aliases) > 0 {
			line += " (" + strings.Join(prefixAll("/", c.Aliases), ", ") + ")"
		}
		line += strings.Repeat(" ", max(1, 24-len(line)))
		line += c.Summary
		b.WriteString(line + "\n")
	}
	return Result{Output: strings.TrimRight(b.String(), "\n")}
}

func configHandler(ctx Context, _ []string) Result {
	c := ctx.Config
	presetLine := fmt.Sprintf("llm preset:   %s", presetLabel(c.LLMServer))
	return Result{Output: strings.Join([]string{
		fmt.Sprintf("mode:         %s", safeMode(c.Mode)),
		fmt.Sprintf("language:     %s", orDefault(c.Language, config.LanguageKorean)),
		fmt.Sprintf("sidecar path: %s", orDefault(c.Sidecar.Path, "(auto-resolved)")),
		presetLine,
		fmt.Sprintf("llm url:      %s", c.LLMServer.URL),
		fmt.Sprintf("llm model:    %s", c.LLMServer.Model),
		fmt.Sprintf("remote url:   %s", c.Server.URL),
		fmt.Sprintf("theme:        %s", c.UI.Theme),
		fmt.Sprintf("shell:        %s", c.Shell.Shell),
	}, "\n")}
}

func presetLabel(c config.LLMServerConfig) string {
	if p := config.MatchPresetFromConfig(c); p != nil {
		return p.Label + " (" + p.ID + ")"
	}
	return "(custom — not on preset list)"
}

func sidecarHandler(ctx Context, _ []string) Result {
	if pc, ok := ctx.Client.(*client.ProcessClient); ok {
		return Result{Output: fmt.Sprintf(
			"binary: %s\nplatform: %s/%s",
			pc.BinPath(), runtime.GOOS, runtime.GOARCH,
		)}
	}
	return Result{Output: "sidecar info unavailable (client mode is " + safeMode(ctx.Config.Mode) + ")"}
}

func versionHandler(_ Context, _ []string) Result {
	return Result{Output: fmt.Sprintf(
		"banya-cli\nplatform: %s/%s\ngo: %s",
		runtime.GOOS, runtime.GOARCH, runtime.Version(),
	)}
}

func prefixAll(p string, ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = p + s
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func safeMode(m string) string {
	if m == "" {
		return "sidecar"
	}
	return m
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
