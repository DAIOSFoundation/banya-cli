// Package styles defines the visual theme for the banya TUI.
package styles

import (
	"github.com/charmbracelet/lipgloss"
)

// Theme holds all style definitions for the application.
type Theme struct {
	// App-level
	AppBorder  lipgloss.Style
	Background lipgloss.Color // terminal background color
	Foreground lipgloss.Color // terminal default text color

	// Header / Status bar
	StatusBar    lipgloss.Style
	StatusKey    lipgloss.Style
	StatusValue  lipgloss.Style
	StatusOK     lipgloss.Style
	StatusError  lipgloss.Style

	// Chat area
	UserMessage      lipgloss.Style
	AssistantMessage lipgloss.Style
	SystemMessage    lipgloss.Style
	RoleLabel        lipgloss.Style
	Timestamp        lipgloss.Style

	// Input area
	InputPrompt lipgloss.Style
	InputText   lipgloss.Style
	InputBorder lipgloss.Style

	// Tool calls
	ToolCallBox      lipgloss.Style
	ToolCallName     lipgloss.Style
	ToolCallRunning  lipgloss.Style
	ToolCallDone     lipgloss.Style
	ToolCallFailed   lipgloss.Style
	ToolCallApproval lipgloss.Style

	// File diff
	DiffAdd    lipgloss.Style
	DiffRemove lipgloss.Style
	DiffHeader lipgloss.Style

	// Misc
	Help    lipgloss.Style
	Spinner lipgloss.Style
	Error   lipgloss.Style
	Subtle  lipgloss.Style
	Bold    lipgloss.Style
}

// Hacker theme colors — black background, green text, monospace feel
var (
	colorBg        = lipgloss.Color("#000000") // pure black
	colorFg        = lipgloss.Color("#00FF41") // matrix green
	colorFgBright  = lipgloss.Color("#39FF14") // neon green
	colorFgDim     = lipgloss.Color("#00802a") // dim green
	colorAccent    = lipgloss.Color("#00FFFF") // cyan
	colorHighlight = lipgloss.Color("#ADFF2F") // green-yellow
	colorWarn      = lipgloss.Color("#FFD700") // gold
	colorDanger    = lipgloss.Color("#FF3131") // red
	colorSurface   = lipgloss.Color("#0a0a0a") // near-black surface
	colorBorder    = lipgloss.Color("#00802a") // dim green border
	colorDiffAddBg = lipgloss.Color("#002200") // very dark green
	colorDiffDelBg = lipgloss.Color("#220000") // very dark red
)

// DarkTheme returns the default hacker theme (black bg, green text).
func DarkTheme() Theme {
	return Theme{
		Background: colorBg,
		Foreground: colorFg,

		AppBorder: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder),

		StatusBar: lipgloss.NewStyle().
			Padding(0, 1).
			Background(colorSurface).
			Foreground(colorFg),
		StatusKey: lipgloss.NewStyle().
			Foreground(colorFgDim).
			Bold(true),
		StatusValue: lipgloss.NewStyle().
			Foreground(colorFg),
		StatusOK: lipgloss.NewStyle().
			Foreground(colorFgBright),
		StatusError: lipgloss.NewStyle().
			Foreground(colorDanger),

		UserMessage: lipgloss.NewStyle().
			Foreground(colorFg).
			Padding(0, 1),
		AssistantMessage: lipgloss.NewStyle().
			Foreground(colorFg).
			Padding(0, 1),
		SystemMessage: lipgloss.NewStyle().
			Foreground(colorFgDim).
			Italic(true).
			Padding(0, 1),
		RoleLabel: lipgloss.NewStyle().
			Bold(true).
			Foreground(colorFgBright),
		Timestamp: lipgloss.NewStyle().
			Foreground(colorFgDim),

		InputPrompt: lipgloss.NewStyle().
			Foreground(colorFgBright).
			Bold(true),
		InputText: lipgloss.NewStyle().
			Foreground(colorFg),
		InputBorder: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Padding(0, 1),

		ToolCallBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorBorder).
			Foreground(colorFg).
			Padding(0, 1).
			MarginLeft(2),
		ToolCallName: lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true),
		ToolCallRunning: lipgloss.NewStyle().
			Foreground(colorWarn),
		ToolCallDone: lipgloss.NewStyle().
			Foreground(colorFgBright),
		ToolCallFailed: lipgloss.NewStyle().
			Foreground(colorDanger),
		ToolCallApproval: lipgloss.NewStyle().
			Foreground(colorWarn).
			Bold(true),

		DiffAdd: lipgloss.NewStyle().
			Background(colorDiffAddBg).
			Foreground(colorFgBright),
		DiffRemove: lipgloss.NewStyle().
			Background(colorDiffDelBg).
			Foreground(colorDanger),
		DiffHeader: lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true),

		Help: lipgloss.NewStyle().
			Foreground(colorFgDim),
		Spinner: lipgloss.NewStyle().
			Foreground(colorFgBright),
		Error: lipgloss.NewStyle().
			Foreground(colorDanger).
			Bold(true),
		Subtle: lipgloss.NewStyle().
			Foreground(colorFgDim),
		Bold: lipgloss.NewStyle().
			Foreground(colorFg).
			Bold(true),
	}
}
