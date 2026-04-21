package components

import (
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cascadecodes/banya-cli/internal/ui/styles"
)

// Powerline prompt colors
var (
	promptBg  = lipgloss.Color("#00AAAA")
	promptFg  = lipgloss.Color("#000000")
	promptSep = lipgloss.Color("#00AAAA")
)

const plArrow = "▶"

// InputModel manages the user text input area.
type InputModel struct {
	textarea textarea.Model
	theme    styles.Theme
	focused  bool
}

// NewInputModel creates a new input model with powerline prompt style.
func NewInputModel(theme styles.Theme) InputModel {
	ta := textarea.New()
	ta.Placeholder = "메시지를 입력하세요..."
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.Focus()
	ta.CharLimit = 0

	ta.FocusedStyle.Base = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00FF41"))
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00802a"))

	ta.BlurredStyle.Base = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00802a"))
	ta.BlurredStyle.Placeholder = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#004415"))

	ta.Cursor.Style = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#39FF14"))

	return InputModel{
		textarea: ta,
		theme:    theme,
		focused:  true,
	}
}

// Init satisfies the tea.Model interface.
func (m InputModel) Init() tea.Cmd {
	return textarea.Blink
}

// Update processes input events.
func (m InputModel) Update(msg tea.Msg) (InputModel, tea.Cmd) {
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// View renders a two-line Oh My Posh style prompt.
// Line 1: powerline segment
// Line 2: ⚡❯ input
func (m InputModel) View() string {
	// Line 1: powerline segment bar
	seg := lipgloss.NewStyle().
		Background(promptBg).Foreground(promptFg).Bold(true).
		Padding(0, 1).
		Render("⚡ input")
	sep := lipgloss.NewStyle().
		Foreground(promptSep).
		Render(plArrow)
	line1 := seg + sep

	// Line 2: prompt symbol + textarea
	var symbol string
	if m.focused {
		symbol = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#39FF14")).Bold(true).
			Render(" ⚡❯ ")
	} else {
		symbol = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00802a")).
			Render(" ⚡❯ ")
	}
	line2 := symbol + m.textarea.View()

	return line1 + "\n" + line2
}

// Value returns the current text.
func (m InputModel) Value() string {
	return m.textarea.Value()
}

// Reset clears the input.
func (m *InputModel) Reset() {
	m.textarea.Reset()
}

// SetValue replaces the textarea content and positions the cursor at
// the end. Used by slash-command autocomplete to expand a partial
// "/mo" into "/model ".
func (m *InputModel) SetValue(v string) {
	m.textarea.SetValue(v)
	m.textarea.CursorEnd()
}

// SetWidth updates the textarea width.
func (m *InputModel) SetWidth(width int) {
	m.textarea.SetWidth(width - 6) // account for " ⚡❯ "
}

// Focus sets focus on the input.
func (m *InputModel) Focus() tea.Cmd {
	m.focused = true
	return m.textarea.Focus()
}

// Blur removes focus from the input.
func (m *InputModel) Blur() {
	m.focused = false
	m.textarea.Blur()
}
