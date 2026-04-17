package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// debugBufferCap is the ring-buffer capacity for the debug panel.
const debugBufferCap = 500

// defaultDebugPanelHeight is how many rows the debug panel occupies when
// open (including its header + footer padding).
const defaultDebugPanelHeight = 10

// debugBuffer is a simple ring buffer of lines with timestamps. Safe to
// mutate from the Update loop only (Bubble Tea is single-threaded there).
type debugBuffer struct {
	lines []string
	cap   int
}

func newDebugBuffer() *debugBuffer {
	return &debugBuffer{cap: debugBufferCap}
}

// push appends a line. Multi-line strings are split on '\n'.
func (b *debugBuffer) push(level, content string) {
	now := time.Now().Format("15:04:05.000")
	for _, piece := range strings.Split(content, "\n") {
		if piece == "" {
			continue
		}
		line := fmt.Sprintf("%s %s %s", now, level, piece)
		b.lines = append(b.lines, line)
		if len(b.lines) > b.cap {
			b.lines = b.lines[len(b.lines)-b.cap:]
		}
	}
}

// tail returns the last n lines joined with newlines.
func (b *debugBuffer) tail(n int) string {
	start := len(b.lines) - n
	if start < 0 {
		start = 0
	}
	return strings.Join(b.lines[start:], "\n")
}

// renderDebugPanel draws the debug panel with a bordered box at the
// bottom of the layout. width/height are outer dimensions (height
// includes header + border).
func renderDebugPanel(b *debugBuffer, width, height int) string {
	if height < 3 {
		height = 3
	}
	bodyRows := height - 2 // leave room for header + top border
	if bodyRows < 1 {
		bodyRows = 1
	}

	header := lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a2e")).
		Foreground(lipgloss.Color("#FFD700")).
		Bold(true).
		Padding(0, 1).
		Width(width).
		Render("▼ debug — Ctrl+T to close")

	bodyStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("#0a0a0a")).
		Foreground(lipgloss.Color("#bfbfbf")).
		Width(width).
		Height(bodyRows)

	content := b.tail(bodyRows)
	if content == "" {
		content = lipgloss.NewStyle().Faint(true).Render("  (no activity yet — CoT tokens and sidecar events will appear here)")
	}
	body := bodyStyle.Render(content)

	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}
