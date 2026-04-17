package commands

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/google/uuid"
)

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
	return Result{Output: strings.Join([]string{
		fmt.Sprintf("mode:         %s", safeMode(c.Mode)),
		fmt.Sprintf("sidecar path: %s", orDefault(c.Sidecar.Path, "(auto-resolved)")),
		fmt.Sprintf("llm url:      %s", c.LLMServer.URL),
		fmt.Sprintf("llm model:    %s", c.LLMServer.Model),
		fmt.Sprintf("remote url:   %s", c.Server.URL),
		fmt.Sprintf("theme:        %s", c.UI.Theme),
		fmt.Sprintf("shell:        %s", c.Shell.Shell),
	}, "\n")}
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
