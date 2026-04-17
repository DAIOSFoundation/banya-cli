package components

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/cascadecodes/banya-cli/internal/ui/styles"
)

// Powerline separator characters (require Nerd Font / Powerline-patched font)
const (
	plRight = "\ue0b0" //  right arrow separator
	plLeft  = "\ue0b2" //  left arrow separator
)

// Segment colors
var (
	// Segment 1: app name — cyan bg
	seg1Bg = lipgloss.Color("#00AAAA")
	seg1Fg = lipgloss.Color("#000000")

	// Segment 2: directory — dark bg
	seg2Bg = lipgloss.Color("#1a1a2e")
	seg2Fg = lipgloss.Color("#00FF41")

	// Segment 3: connection — green/red bg
	seg3BgOK  = lipgloss.Color("#00802a")
	seg3BgErr = lipgloss.Color("#8B0000")
	seg3Fg    = lipgloss.Color("#000000")

	// Segment 4: session — gold bg
	seg4Bg = lipgloss.Color("#B8860B")
	seg4Fg = lipgloss.Color("#000000")

	// Segment 5: time — dark bg
	seg5Bg = lipgloss.Color("#0a0a0a")
	seg5Fg = lipgloss.Color("#00FF41")
)

// StatusBar renders the Oh My Posh–style powerline status bar.
type StatusBar struct {
	theme      styles.Theme
	width      int
	connected  bool
	model      string
	session    string
	tokens     int
	promptMode string
}

// NewStatusBar creates a new status bar.
func NewStatusBar(theme styles.Theme) StatusBar {
	return StatusBar{
		theme: theme,
		model: "-",
	}
}

func (s *StatusBar) SetWidth(w int)             { s.width = w }
func (s *StatusBar) SetConnected(c bool)        { s.connected = c }
func (s *StatusBar) SetModel(m string)          { s.model = m }
func (s *StatusBar) SetSession(ss string)       { s.session = ss }
func (s *StatusBar) SetTokens(t int)            { s.tokens = t }
func (s *StatusBar) SetPromptMode(m string)     { s.promptMode = m }

// View renders the powerline status bar.
func (s StatusBar) View() string {
	// --- Build segments ---

	// Seg1: app name
	seg1Content := lipgloss.NewStyle().
		Background(seg1Bg).Foreground(seg1Fg).Bold(true).
		Padding(0, 1).
		Render(" ⚡ banya ")
	seg1Sep := lipgloss.NewStyle().
		Foreground(seg1Bg).Background(seg2Bg).
		Render(plRight)

	// Seg2: working directory
	wd := shortPath()
	seg2Content := lipgloss.NewStyle().
		Background(seg2Bg).Foreground(seg2Fg).
		Padding(0, 1).
		Render(" " + wd)

	seg3Bg := seg3BgOK
	connIcon := "●"
	connText := "connected"
	if !s.connected {
		seg3Bg = seg3BgErr
		connIcon = "○"
		connText = "disconnected"
	}
	seg2Sep := lipgloss.NewStyle().
		Foreground(seg2Bg).Background(seg3Bg).
		Render(plRight)

	// Seg3: connection status
	seg3Content := lipgloss.NewStyle().
		Background(seg3Bg).Foreground(seg3Fg).Bold(true).
		Padding(0, 1).
		Render(connIcon + " " + connText)
	seg3Sep := lipgloss.NewStyle().
		Foreground(seg3Bg).Background(seg4Bg).
		Render(plRight)

	// Seg4: session
	sessID := truncate(s.session, 8)
	seg4Content := lipgloss.NewStyle().
		Background(seg4Bg).Foreground(seg4Fg).
		Padding(0, 1).
		Render("⏍ " + sessID)

	// Seg5: prompt mode (purple bg)
	modeBg := lipgloss.Color("#5e3ec5")
	modeFg := lipgloss.Color("#FFFFFF")
	mode := s.promptMode
	if mode == "" {
		mode = "agent"
	}
	modeIcon := promptModeIcon(mode)
	seg5ModeContent := lipgloss.NewStyle().
		Background(modeBg).Foreground(modeFg).Bold(true).
		Padding(0, 1).
		Render(modeIcon + " " + mode)
	seg4ToModeSep := lipgloss.NewStyle().
		Foreground(seg4Bg).Background(modeBg).
		Render(plRight)
	modeToTimeSep := lipgloss.NewStyle().
		Foreground(modeBg).Background(seg5Bg).
		Render(plRight)

	// Seg6: time
	now := time.Now().Format("15:04")
	seg5Content := lipgloss.NewStyle().
		Background(seg5Bg).Foreground(seg5Fg).
		Padding(0, 1).
		Render("⏰ " + now)
	seg5Sep := lipgloss.NewStyle().
		Foreground(seg5Bg).
		Render(plRight)

	left := seg1Content + seg1Sep +
		seg2Content + seg2Sep +
		seg3Content + seg3Sep +
		seg4Content + seg4ToModeSep +
		seg5ModeContent + modeToTimeSep +
		seg5Content + seg5Sep

	// Fill remaining width with black
	leftWidth := lipgloss.Width(left)
	remaining := s.width - leftWidth
	if remaining < 0 {
		remaining = 0
	}
	fill := strings.Repeat(" ", remaining)

	return left + fill
}

// promptModeIcon returns a glyph for each prompt mode.
func promptModeIcon(mode string) string {
	switch mode {
	case "agent":
		return "🤖"
	case "code":
		return "</>"
	case "plan":
		return "📋"
	case "ask":
		return "💬"
	}
	return "•"
}

// shortPath returns a shortened working directory path.
func shortPath() string {
	wd, err := os.Getwd()
	if err != nil {
		return "~"
	}
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(wd, home) {
		wd = "~" + wd[len(home):]
	}
	// Shorten to last 2 components if long
	parts := strings.Split(wd, string(filepath.Separator))
	if len(parts) > 3 {
		wd = "…/" + strings.Join(parts[len(parts)-2:], "/")
	}
	return wd
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
