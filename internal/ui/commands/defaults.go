package commands

import (
	"fmt"
	"os"
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
		Summary: "Save the current conversation, then start a fresh session",
		Handler: func(ctx Context, _ []string) Result {
			if ctx.SaveCurrentAndStartNew == nil {
				// No session manager wired — legacy behaviour: just
				// clear and mint a new id.
				return Result{Clear: true, Output: "new session: " + uuid.New().String()}
			}
			newID := ctx.SaveCurrentAndStartNew()
			return Result{
				Clear:  true,
				Output: "saved previous conversation · new session: " + newID,
			}
		},
	})
	r.Register(&Command{
		Name:    "sessions",
		Aliases: []string{"ls-sessions"},
		Usage:   "/sessions",
		Summary: "List saved conversations for this workspace (most recent first)",
		Handler: sessionsHandler,
	})
	r.Register(&Command{
		Name:    "load",
		Usage:   "/load <session-id|index>",
		Summary: "Load a saved conversation by id (full UUID or 1-based index from /sessions)",
		Handler: loadSessionHandler,
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
	r.Register(&Command{
		Name:    "key",
		Aliases: []string{"apikey"},
		Usage:   "/key [preset-id] <value>  or  /key [preset-id] --clear",
		Summary: "Save / clear the API key for an LLM preset (persisted in ~/.config/banya/keys.json, 0600)",
		Handler: keyHandler,
	})
}

// keyHandler saves an API key for an LLM preset into keys.json (mode
// 0600 under the config dir). With one arg, the key is stored for the
// CURRENTLY-SELECTED preset; with two args the first is the preset ID.
// Pass `--clear` in place of the value to delete the entry.
//
// env var shadowing: the subsequent Resolve() still prefers
// os.Getenv(APIKeyEnv) over the stored file, so an explicit `export`
// in the shell trumps anything the user typed here. That matches
// 12-factor habit and keeps the TUI from silently using a stale key
// when the operator has re-exported a new one.
func keyHandler(ctx Context, args []string) Result {
	if len(args) == 0 {
		return Result{Output: "usage: /key [preset-id] <value>  or  /key [preset-id] --clear\n" +
			"Without <preset-id> the current preset is used."}
	}

	var presetID, value string
	if len(args) == 1 {
		current := config.MatchPresetFromConfig(ctx.Config.LLMServer)
		if current == nil {
			return Result{Output: "No preset currently selected — run `/model <id>` first, then `/key <value>`."}
		}
		presetID = current.ID
		value = args[0]
	} else {
		presetID = strings.ToLower(args[0])
		value = args[1]
	}

	p := config.LookupPreset(presetID)
	if p == nil {
		return Result{Output: "unknown preset: " + presetID + "  (try /model to list)"}
	}
	if p.APIKeyEnv == "" {
		return Result{Output: "preset " + presetID + " doesn't use an API key (" + p.Label + ")"}
	}

	if value == "--clear" || value == "-c" {
		if err := config.SaveLLMKey(presetID, ""); err != nil {
			return Result{Output: "failed to clear key: " + err.Error()}
		}
		return Result{Output: "cleared stored key for " + presetID + " (env " + p.APIKeyEnv + " still wins if exported)"}
	}

	if err := config.SaveLLMKey(presetID, value); err != nil {
		return Result{Output: "failed to save key: " + err.Error()}
	}

	envShadow := ""
	if v := os.Getenv(p.APIKeyEnv); v != "" {
		envShadow = "\n(note: env var " + p.APIKeyEnv + " is set — it will still override the stored key)"
	}
	return Result{Output: "saved " + presetID + " API key to ~/.config/banya/keys.json (0600)" + envShadow +
		"\nRun `/model " + presetID + "` to switch the main LLM to this preset."}
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

// sessionsHandler powers `/sessions`. Lists saved conversations for
// the current workspace, most recent first, with a 1-based index that
// `/load <index>` accepts. Empty state and missing-callback errors
// fall through to helpful messages rather than noisy stack traces.
func sessionsHandler(ctx Context, _ []string) Result {
	if ctx.ListSessions == nil {
		return Result{Output: "session manager not wired in this client"}
	}
	sessions := ctx.ListSessions()
	if len(sessions) == 0 {
		return Result{Output: "no saved sessions in this workspace yet · start chatting and one will be saved automatically"}
	}
	var b strings.Builder
	b.WriteString("Saved sessions (use `/load <n>` or `/load <uuid>`):\n")
	for i, s := range sessions {
		marker := "  "
		if s.Current {
			marker = "* "
		}
		when := s.UpdatedAt.Local().Format("2006-01-02 15:04")
		preview := s.Preview
		if preview == "" {
			preview = "(no user message yet)"
		}
		if len(preview) > 60 {
			preview = preview[:57] + "…"
		}
		fmt.Fprintf(&b, "%s%d. %s · %s · %d msgs\n     %s\n",
			marker, i+1, shortUUID(s.ID), when, s.MessageCount, preview)
	}
	return Result{Output: strings.TrimRight(b.String(), "\n")}
}

// loadSessionHandler powers `/load <id|index>`. Accepts either a full
// UUID or a 1-based index matching `/sessions`. Anything else falls
// through with a usage hint.
func loadSessionHandler(ctx Context, args []string) Result {
	if len(args) == 0 {
		return Result{Output: "usage: /load <session-id|index>  (see /sessions for the list)"}
	}
	if ctx.LoadSession == nil || ctx.ListSessions == nil {
		return Result{Output: "session manager not wired in this client"}
	}
	arg := strings.TrimSpace(args[0])
	var targetID string
	// Try index first: 1-based into the ListSessions slice.
	if idx, err := parsePresetIndex(arg); err == nil {
		sessions := ctx.ListSessions()
		if idx < 0 || idx >= len(sessions) {
			return Result{Output: fmt.Sprintf(
				"index %s out of range — /sessions has %d entries", arg, len(sessions),
			)}
		}
		targetID = sessions[idx].ID
	} else {
		targetID = arg
	}
	if err := ctx.LoadSession(targetID); err != nil {
		return Result{Output: "load failed: " + err.Error()}
	}
	return Result{Output: "loaded session " + shortUUID(targetID)}
}

// shortUUID trims a UUID to its first 8 chars for compact display.
// Falls back to the full string when it's shorter than 8 chars.
func shortUUID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}
