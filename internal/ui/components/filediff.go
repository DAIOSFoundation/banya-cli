package components

import (
	"fmt"
	"strings"

	"github.com/cascadecodes/banya-cli/internal/ui/styles"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// FileDiffView renders file change diffs.
type FileDiffView struct {
	theme styles.Theme
}

// NewFileDiffView creates a new diff renderer.
func NewFileDiffView(theme styles.Theme) FileDiffView {
	return FileDiffView{theme: theme}
}

// RenderFileChange renders a single file change.
func (f *FileDiffView) RenderFileChange(change protocol.FileChange) string {
	var b strings.Builder

	// Header
	var actionLabel string
	switch change.Action {
	case "create":
		actionLabel = f.theme.ToolCallDone.Render("[created]")
	case "modify":
		actionLabel = f.theme.ToolCallRunning.Render("[modified]")
	case "delete":
		actionLabel = f.theme.ToolCallFailed.Render("[deleted]")
	default:
		actionLabel = f.theme.Subtle.Render("[" + change.Action + "]")
	}

	header := fmt.Sprintf("%s %s",
		f.theme.DiffHeader.Render(change.Path),
		actionLabel,
	)
	b.WriteString(header)
	b.WriteString("\n")

	// Diff content
	if change.Diff != "" {
		lines := strings.Split(change.Diff, "\n")
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line, "+"):
				b.WriteString(f.theme.DiffAdd.Render(line))
			case strings.HasPrefix(line, "-"):
				b.WriteString(f.theme.DiffRemove.Render(line))
			case strings.HasPrefix(line, "@@"):
				b.WriteString(f.theme.DiffHeader.Render(line))
			default:
				b.WriteString(line)
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// RenderFileChanges renders a list of file changes.
func (f *FileDiffView) RenderFileChanges(changes []protocol.FileChange) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, change := range changes {
		b.WriteString(f.RenderFileChange(change))
	}
	return b.String()
}
