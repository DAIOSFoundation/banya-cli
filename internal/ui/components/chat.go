package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/cascadecodes/banya-cli/internal/ui/styles"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/charmbracelet/glamour"
)

// ChatView renders the conversation messages.
type ChatView struct {
	theme    styles.Theme
	renderer *glamour.TermRenderer
	width    int
}

// NewChatView creates a new chat renderer.
func NewChatView(theme styles.Theme, width int) *ChatView {
	r, _ := glamour.NewTermRenderer(
		glamour.WithStylePath("dark"),
		glamour.WithWordWrap(width-4),
	)
	return &ChatView{
		theme:    theme,
		renderer: r,
		width:    width,
	}
}

// SetWidth updates the rendering width.
func (c *ChatView) SetWidth(width int) {
	c.width = width
	c.renderer, _ = glamour.NewTermRenderer(
		glamour.WithStylePath("dark"),
		glamour.WithWordWrap(width-4),
	)
}

// RenderMessage formats a single message for display.
func (c *ChatView) RenderMessage(msg protocol.Message) string {
	var b strings.Builder

	switch msg.Role {
	case protocol.RoleUser:
		label := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00FFFF")).Render("You")
		ts := c.theme.Timestamp.Render(msg.CreatedAt.Format("15:04"))
		b.WriteString(fmt.Sprintf("%s %s\n", label, ts))
	case protocol.RoleAssistant:
		label := c.theme.RoleLabel.Render("Banya")
		ts := c.theme.Timestamp.Render(msg.CreatedAt.Format("15:04"))
		b.WriteString(fmt.Sprintf("%s %s\n", label, ts))
	case protocol.RoleSystem:
		b.WriteString(c.theme.SystemMessage.Render(msg.Content))
		b.WriteString("\n")
		return b.String()
	}

	if msg.Content != "" {
		rendered, err := c.renderer.Render(msg.Content)
		if err != nil {
			b.WriteString(msg.Content)
		} else {
			b.WriteString(strings.TrimSpace(rendered))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// RenderMessages formats all messages in the conversation.
func (c *ChatView) RenderMessages(messages []protocol.Message) string {
	var b strings.Builder
	separator := lipgloss.NewStyle().Foreground(lipgloss.Color("#00802a")).Render(strings.Repeat("─", c.width-2))
	for i, msg := range messages {
		b.WriteString(c.RenderMessage(msg))
		if i < len(messages)-1 {
			b.WriteString(separator)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// RenderStreamingContent renders content that is still being streamed.
func (c *ChatView) RenderStreamingContent(content string) string {
	var b strings.Builder
	label := c.theme.RoleLabel.Render("Banya")
	typing := lipgloss.NewStyle().Foreground(lipgloss.Color("#00802a")).Render("typing...")
	b.WriteString(fmt.Sprintf("%s %s\n", label, typing))

	if content != "" {
		rendered, err := c.renderer.Render(content)
		if err != nil {
			b.WriteString(content)
		} else {
			b.WriteString(strings.TrimSpace(rendered))
		}
	}
	cursor := lipgloss.NewStyle().Foreground(lipgloss.Color("#39FF14")).Render("_")
	b.WriteString(cursor)
	b.WriteString("\n")
	return b.String()
}
