package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// thinkTickInterval controls the cadence of the "thinking..." animation.
const thinkTickInterval = 300 * time.Millisecond

// thinkTickMsg drives the dots animation in the thinking placeholder.
type thinkTickMsg struct{}

func thinkTick() tea.Cmd {
	return tea.Tick(thinkTickInterval, func(time.Time) tea.Msg {
		return thinkTickMsg{}
	})
}
