// Package commands implements TUI slash commands (e.g. /help, /sessions).
//
// A slash command is any user input starting with `/`. Commands are
// dispatched synchronously in the Bubble Tea Update loop: the handler
// receives the parsed args plus a Context that exposes the shared
// client and config, and returns a Result describing what the UI
// should render. Long-running work should be wrapped in a tea.Cmd
// via Result.Cmd.
package commands

import (
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// Context bundles the state a command handler may need.
type Context struct {
	Client    client.Client
	Config    *config.Config
	SessionID string

	// PromptMode returns the currently active prompt mode.
	PromptMode func() protocol.PromptType
	// SetPromptMode changes the active prompt mode. Returns an error for
	// invalid values; valid ones are code / ask / plan / agent.
	SetPromptMode func(protocol.PromptType) error

	// SetLanguage updates the in-memory + on-disk default language. Valid
	// values are normalised codes (ko | en). Returns an error on invalid
	// input or disk failure.
	SetLanguage func(lang string) error

	// ApplyLLMPreset swaps the main-agent backend at runtime. Persists
	// the preset to config.yaml (minus secrets) and hot-swaps the
	// ProcessClient's LLMBackend so the next llm.chat routes to the new
	// endpoint without restart. Returns an error if the preset is
	// unknown or its API-key env var is empty.
	ApplyLLMPreset func(presetID string) error

	// ListSessions returns saved conversations for the current workdir,
	// most recent first. Returns an empty slice (never nil) when nothing
	// is saved or the session dir is unreadable. Powers `/sessions`.
	ListSessions func() []SessionInfo

	// LoadSession switches the Model's in-memory conversation to the
	// saved session with the given ID. Pass an empty ID to start a
	// fresh session (same as `/new`). Returns an error when the ID
	// isn't found. Powers `/load`.
	LoadSession func(id string) error

	// SaveCurrentAndStartNew persists the active conversation, then
	// rolls to a brand-new session id with empty history. Powers
	// `/new`.
	SaveCurrentAndStartNew func() string
}

// SessionInfo is the command-layer projection of an on-disk session
// — enough to render a picker without leaking the session package's
// full shape.
type SessionInfo struct {
	ID        string
	UpdatedAt time.Time
	WorkDir   string
	// Preview is a short excerpt of the first user message, handy for
	// identifying sessions in the /sessions list.
	Preview string
	// MessageCount is the total message count (user + assistant + system).
	MessageCount int
	// Current flags the session that's currently loaded in the Model.
	Current bool
}

// Result describes what the UI should show after a command runs.
// Output is rendered as a system message; Cmd (optional) runs after.
type Result struct {
	Output string
	Quit   bool
	Clear  bool
	Cmd    tea.Cmd
}

// Handler is the function signature for a slash command.
type Handler func(ctx Context, args []string) Result

// Command describes a single /command entry.
type Command struct {
	Name    string
	Aliases []string
	Summary string
	Usage   string
	Handler Handler
}

// Registry holds every known slash command.
type Registry struct {
	commands map[string]*Command
	order    []string
}

// NewRegistry builds a Registry pre-populated with the default command set.
func NewRegistry() *Registry {
	r := &Registry{commands: make(map[string]*Command)}
	r.registerDefaults()
	return r
}

// Register adds a command. Aliases point back to the primary entry.
func (r *Registry) Register(cmd *Command) {
	r.commands[cmd.Name] = cmd
	r.order = append(r.order, cmd.Name)
	for _, a := range cmd.Aliases {
		r.commands[a] = cmd
	}
}

// All returns every primary command in registration order.
func (r *Registry) All() []*Command {
	out := make([]*Command, 0, len(r.order))
	seen := map[string]bool{}
	for _, name := range r.order {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, r.commands[name])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds a command by name or alias (sans leading '/').
func (r *Registry) Lookup(name string) (*Command, bool) {
	c, ok := r.commands[strings.ToLower(name)]
	return c, ok
}

// IsSlashCommand reports whether the input looks like a /command line.
func IsSlashCommand(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "/") && len(s) > 1
}

// Parse splits a "/cmd arg1 arg2" line into (name, args).
func Parse(line string) (name string, args []string) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "/"))
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return "", nil
	}
	return strings.ToLower(parts[0]), parts[1:]
}

// Dispatch runs the named command. Returns a user-friendly error Result
// if the command is unknown.
func (r *Registry) Dispatch(line string, ctx Context) Result {
	name, args := Parse(line)
	if name == "" {
		return Result{Output: "empty command"}
	}
	cmd, ok := r.Lookup(name)
	if !ok {
		return Result{Output: "unknown command: /" + name + " (try /help)"}
	}
	return cmd.Handler(ctx, args)
}
