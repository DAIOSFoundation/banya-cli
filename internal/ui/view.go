package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Powerline-style separator. Standard Unicode glyph so it renders in any
// monospace font — the previous `\ue0b0` required a Nerd Font and showed
// up as `?` without one.
const plSep = "▶"

// Base style for the full-screen black background.
var baseStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("#000000")).
	Foreground(lipgloss.Color("#00FF41"))

// View renders the complete TUI layout.
func (m Model) View() string {
	if !m.ready {
		return baseStyle.Width(m.width).Height(m.height).Render("\n  Initializing...")
	}

	var sections []string

	// Status bar (top) — powerline segments
	sections = append(sections, m.statusBar.View())

	// Welcome banner or main content
	if m.state == StateSettings && m.settings != nil {
		sections = append(sections, m.settings.View())
	} else if m.showBanner && len(m.messages) == 0 && m.state == StateReady {
		headerHeight := 1
		inputHeight := 3 // 2-line prompt + 1
		helpHeight := 1
		bannerHeight := m.height - headerHeight - inputHeight - helpHeight - 2
		sections = append(sections, renderBanner(m.width, bannerHeight, m.bannerLines, m.taglineChars))
	} else {
		sections = append(sections, m.viewport.View())
	}

	// Debug panel (Ctrl+T)
	if m.debugOpen {
		sections = append(sections, renderDebugPanel(m.debugBuf, m.width, m.debugHeight))
	}

	// Error display
	if m.lastError != "" {
		errMsg := m.theme.Error.Render(fmt.Sprintf(" Error: %s", m.lastError))
		sections = append(sections, errMsg)
	}

	// Input area or state indicator
	switch m.state {
	case StateReady:
		sections = append(sections, m.input.View())
	case StateStreaming:
		sections = append(sections, renderStreamingBar(m.spinner.View()))
	case StateApproval:
		sections = append(sections, renderApprovalBar())
	case StateSettings:
		// settings 폼이 이미 키 안내를 포함하므로 상태바만 가볍게 표시.
		sections = append(sections, renderSettingsBar())
	}

	// Help line — powerline style
	help := renderHelpBar(m.state, m.width)
	sections = append(sections, help)

	body := lipgloss.JoinVertical(lipgloss.Left, sections...)

	return baseStyle.Width(m.width).Height(m.height).Render(body)
}

// renderStreamingBar shows a powerline-style streaming indicator.
func renderStreamingBar(spinnerView string) string {
	seg := lipgloss.NewStyle().
		Background(lipgloss.Color("#B8860B")).Foreground(lipgloss.Color("#000000")).Bold(true).
		Padding(0, 1).
		Render("⚡ streaming")
	sep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#B8860B")).
		Render(plSep)
	detail := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00FF41")).
		Render("  " + spinnerView + " Receiving response...")
	return seg + sep + detail
}

// renderSettingsBar shows a powerline-style banner while the settings
// form is open.
func renderSettingsBar() string {
	seg := lipgloss.NewStyle().
		Background(lipgloss.Color("#1e3a8a")).Foreground(lipgloss.Color("#ffffff")).Bold(true).
		Padding(0, 1).
		Render("⚙ 설정")
	sep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#1e3a8a")).
		Render(plSep)
	detail := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00FF41")).
		Render("  Subagent / critic 모델 설정 중 · Esc 로 취소")
	return seg + sep + detail
}

// renderApprovalBar shows a powerline-style approval prompt.
func renderApprovalBar() string {
	seg := lipgloss.NewStyle().
		Background(lipgloss.Color("#8B0000")).Foreground(lipgloss.Color("#FFD700")).Bold(true).
		Padding(0, 1).
		Render("⚠ 승인 필요")
	sep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#8B0000")).
		Render(plSep)
	detail := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FFD700")).Bold(true).
		Render("  Enter: 승인  |  Esc: 거부")
	return seg + sep + detail
}

// renderHelpBar shows keyboard shortcuts in powerline style.
func renderHelpBar(state State, width int) string {
	var keys []string
	switch state {
	case StateReady:
		keys = []string{"Enter:전송", "Ctrl+T:디버그", "/settings:모델설정", "/clear:초기화", "/quit:종료", "Ctrl+C:종료"}
	case StateStreaming:
		keys = []string{"Ctrl+T:디버그", "Ctrl+C:종료"}
	case StateApproval:
		keys = []string{"Enter:승인", "Esc:거부", "Ctrl+T:디버그", "Ctrl+C:종료"}
	case StateSettings:
		keys = []string{"Tab:다음필드", "Enter:확인/Submit", "Esc:취소", "Ctrl+C:종료"}
	}

	seg := lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a2e")).Foreground(lipgloss.Color("#00802a")).
		Padding(0, 1).
		Render(strings.Join(keys, "  │  "))
	sep := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#1a1a2e")).
		Render(plSep)

	left := seg + sep
	leftW := lipgloss.Width(left)
	remaining := width - leftW
	if remaining < 0 {
		remaining = 0
	}
	return left + strings.Repeat(" ", remaining)
}

// renderApprovalPrompt renders the approval request in the viewport.
func (m Model) renderApprovalPrompt() string {
	if m.pendingApproval == nil {
		return ""
	}

	ar := m.pendingApproval
	var b strings.Builder

	b.WriteString(m.theme.ToolCallApproval.Render("  ⚠ 승인 필요"))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  도구: %s\n", m.theme.ToolCallName.Render(ar.ToolName)))

	if ar.Description != "" {
		b.WriteString(fmt.Sprintf("  설명: %s\n", ar.Description))
	}
	if ar.Command != "" {
		b.WriteString(fmt.Sprintf("  명령: %s\n", m.theme.Bold.Render(ar.Command)))
	}
	if ar.Risk != "" {
		riskStyle := m.theme.Subtle
		switch ar.Risk {
		case "high":
			riskStyle = m.theme.Error
		case "medium":
			riskStyle = m.theme.ToolCallApproval
		}
		b.WriteString(fmt.Sprintf("  위험도: %s\n", riskStyle.Render(ar.Risk)))
	}

	box := m.theme.ToolCallBox.
		BorderForeground(lipgloss.Color("#FFD700")).
		Render(b.String())
	return box
}
