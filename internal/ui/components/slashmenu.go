package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/cascadecodes/banya-cli/internal/ui/commands"
)

// SlashMenuItem is a projection of commands.Command tailored for menu
// rendering — keeps the UI layer from growing its own command model.
type SlashMenuItem struct {
	Name    string
	Usage   string
	Summary string
}

// FilterSlashCommands returns the ordered subset of `all` whose Name
// matches `input`'s prefix after the leading '/'. Empty input or input
// that doesn't start with '/' returns nil — caller should hide the menu.
// Matching is case-insensitive and considers the command's primary
// Name only (aliases aren't individually listed; they'd be redundant).
func FilterSlashCommands(input string, all []*commands.Command) []SlashMenuItem {
	trimmed := strings.TrimLeft(input, " \t")
	if !strings.HasPrefix(trimmed, "/") {
		return nil
	}
	// Everything after '/' up to the first space is the partial name.
	// If the user already typed a space (arg separator), they've chosen
	// a command — hide the menu so it doesn't obscure args entry.
	body := trimmed[1:]
	if strings.ContainsAny(body, " \t") {
		return nil
	}
	body = strings.ToLower(body)

	out := make([]SlashMenuItem, 0, len(all))
	for _, c := range all {
		if c == nil {
			continue
		}
		if body != "" && !strings.HasPrefix(strings.ToLower(c.Name), body) {
			continue
		}
		out = append(out, SlashMenuItem{
			Name:    c.Name,
			Usage:   c.Usage,
			Summary: c.Summary,
		})
	}
	return out
}

// RenderSlashMenu draws a vertical list of candidate commands with the
// `selected` row highlighted. Width is the available terminal width;
// the menu auto-trims long summaries and picks a sensible two-column
// layout. maxRows caps the visible item count so the menu doesn't
// displace the chat viewport in small terminals.
func RenderSlashMenu(items []SlashMenuItem, selected, width, maxRows int) string {
	if len(items) == 0 {
		return ""
	}
	if maxRows <= 0 {
		maxRows = 8
	}
	// Pick a window around `selected` so it stays in view while cycling.
	start := 0
	if len(items) > maxRows {
		start = selected - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > len(items) {
			start = len(items) - maxRows
		}
	}
	end := start + maxRows
	if end > len(items) {
		end = len(items)
	}

	base := lipgloss.NewStyle().
		Background(lipgloss.Color("#000000")).
		Foreground(lipgloss.Color("#9AA0A6"))
	rowSelected := lipgloss.NewStyle().
		Background(lipgloss.Color("#00AAAA")).
		Foreground(lipgloss.Color("#000000")).
		Bold(true)
	cmdCol := lipgloss.NewStyle().Width(18)
	summaryCol := lipgloss.NewStyle()

	if width < 30 {
		width = 30
	}
	summaryWidth := width - 18 - 2 // minus cmd col minus left padding
	if summaryWidth < 10 {
		summaryWidth = 10
	}
	summaryCol = summaryCol.Width(summaryWidth)

	var lines []string
	header := base.Render("  /commands — Tab to complete, ↑↓ select, Esc to close")
	lines = append(lines, header)
	for i := start; i < end; i++ {
		it := items[i]
		left := cmdCol.Render("  /" + it.Name)
		right := summaryCol.Render(truncateRight(it.Summary, summaryWidth))
		row := left + right
		if i == selected {
			row = rowSelected.Render(row)
		} else {
			row = base.Render(row)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

// truncateRight returns s trimmed to at most `max` runes, with a single
// trailing '…' when truncation occurred. Designed for short help text
// where the original cue is still recognisable.
func truncateRight(s string, max int) string {
	if max <= 1 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
