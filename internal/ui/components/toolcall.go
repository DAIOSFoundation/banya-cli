package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/cascadecodes/banya-cli/internal/ui/styles"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// ToolCallView renders tool call status boxes.
type ToolCallView struct {
	theme   styles.Theme
	spinner spinner.Model
}

// NewToolCallView creates a new tool call renderer.
func NewToolCallView(theme styles.Theme) ToolCallView {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = theme.Spinner
	return ToolCallView{
		theme:   theme,
		spinner: s,
	}
}

// Spinner returns the spinner model for updating in the main update loop.
func (t *ToolCallView) Spinner() spinner.Model {
	return t.spinner
}

// SetSpinner updates the spinner state.
func (t *ToolCallView) SetSpinner(s spinner.Model) {
	t.spinner = s
}

// RenderToolCall renders a single tool call status.
func (t *ToolCallView) RenderToolCall(tc protocol.ToolCall) string {
	var statusIcon string
	var nameStyle lipgloss.Style

	switch tc.Status {
	case protocol.ToolCallPending:
		statusIcon = t.theme.Subtle.Render("○")
		nameStyle = t.theme.ToolCallName
	case protocol.ToolCallRunning:
		statusIcon = t.spinner.View()
		nameStyle = t.theme.ToolCallRunning
	case protocol.ToolCallDone:
		statusIcon = t.theme.ToolCallDone.Render("✓")
		nameStyle = t.theme.ToolCallDone
	case protocol.ToolCallFailed:
		statusIcon = t.theme.ToolCallFailed.Render("✗")
		nameStyle = t.theme.ToolCallFailed
	case protocol.ToolCallApproval:
		statusIcon = t.theme.ToolCallApproval.Render("?")
		nameStyle = t.theme.ToolCallApproval
	}

	header := fmt.Sprintf("%s %s", statusIcon, nameStyle.Render(tc.Name))

	var details strings.Builder
	if len(tc.Args) > 0 {
		for k, v := range tc.Args {
			details.WriteString(fmt.Sprintf("  %s: %v\n", t.theme.Subtle.Render(k), v))
		}
	}
	if tc.Result != "" && tc.Status == protocol.ToolCallDone {
		result := tc.Result
		if len(result) > 200 {
			result = result[:200] + "..."
		}
		details.WriteString(fmt.Sprintf("  %s\n", t.theme.Subtle.Render(result)))
	}
	if tc.Error != "" {
		details.WriteString(fmt.Sprintf("  %s\n", t.theme.Error.Render(tc.Error)))
	}

	content := header
	if details.Len() > 0 {
		content += "\n" + strings.TrimRight(details.String(), "\n")
	}

	return t.theme.ToolCallBox.Render(content)
}

// RenderToolCalls renders all active tool calls.
func (t *ToolCallView) RenderToolCalls(calls []protocol.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	for i, tc := range calls {
		b.WriteString(t.RenderToolCall(tc))
		if i < len(calls)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
