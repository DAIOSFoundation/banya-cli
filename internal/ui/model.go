package ui

import (
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/internal/ui/commands"
	"github.com/cascadecodes/banya-cli/internal/ui/components"
	"github.com/cascadecodes/banya-cli/internal/ui/styles"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/google/uuid"
)

// State tracks the current interaction state.
type State int

const (
	StateReady    State = iota // waiting for user input
	StateStreaming             // receiving streamed response
	StateApproval              // waiting for user to approve a tool call
)

// Model is the main Bubble Tea model for the banya TUI.
type Model struct {
	// Core state
	state     State
	sessionID string
	messages  []protocol.Message
	toolCalls []protocol.ToolCall

	// Streaming
	streamContent string
	eventChan     <-chan protocol.ServerEvent

	// Pending approval
	pendingApproval *protocol.ApprovalRequest

	// Dependencies
	client   client.Client
	cfg      *config.Config
	commands *commands.Registry

	// UI components
	input      components.InputModel
	statusBar  components.StatusBar
	chatView   *components.ChatView
	toolView   components.ToolCallView
	diffView   components.FileDiffView
	viewport   viewport.Model
	spinner    spinner.Model

	// Layout
	width  int
	height int
	ready  bool

	// Welcome banner animation
	showBanner    bool
	bannerLines   int // how many drop-down lines visible
	taglineChars  int // how many tagline characters typed (-1 = not started)

	// Theme
	theme styles.Theme

	// Error display
	lastError string

	// Debug panel (Ctrl+T toggle)
	debugOpen   bool
	debugBuf    *debugBuffer
	debugHeight int

	// Thinking animation frame counter (cycles while streaming)
	thinkingFrame int

	// Active prompt mode (code | ask | plan | agent). Selects which
	// system prompt Core composes for each chat turn.
	promptMode protocol.PromptType
}

// New creates a new TUI model.
func New(apiClient client.Client, cfg *config.Config) Model {
	theme := styles.DarkTheme()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = theme.Spinner

	m := Model{
		state:      StateReady,
		sessionID:  uuid.New().String(),
		messages:   make([]protocol.Message, 0),
		toolCalls:  make([]protocol.ToolCall, 0),
		client:     apiClient,
		cfg:        cfg,
		commands:   commands.NewRegistry(),
		input:      components.NewInputModel(theme),
		statusBar:  components.NewStatusBar(theme),
		chatView:   components.NewChatView(theme, 80),
		toolView:   components.NewToolCallView(theme),
		diffView:   components.NewFileDiffView(theme),
		spinner:    s,
		theme:      theme,
		showBanner:   true,
		taglineChars: -1,
		debugBuf:     newDebugBuffer(),
		debugHeight:  defaultDebugPanelHeight,
		promptMode:   resolvePromptMode(cfg.PromptMode),
	}
	m.initStatusFromConfig()
	return m
}

// resolvePromptMode normalises config-provided prompt modes and falls
// back to "agent" for anything unrecognised (including empty).
func resolvePromptMode(raw string) protocol.PromptType {
	switch protocol.PromptType(raw) {
	case protocol.PromptCode, protocol.PromptAsk, protocol.PromptPlan, protocol.PromptAgent:
		return protocol.PromptType(raw)
	}
	return protocol.PromptAgent
}

// initStatusFromConfig pushes the initial prompt mode into the status
// bar so it shows the right segment from first paint.
func (m *Model) initStatusFromConfig() {
	m.statusBar.SetPromptMode(string(m.promptMode))
}

// CurrentPromptMode exposes the active prompt mode for slash commands.
func (m *Model) CurrentPromptMode() protocol.PromptType { return m.promptMode }

// SetPromptMode switches the active prompt mode. Only the four known
// values are accepted.
func (m *Model) SetPromptMode(mode protocol.PromptType) error {
	switch mode {
	case protocol.PromptCode, protocol.PromptAsk, protocol.PromptPlan, protocol.PromptAgent:
		m.promptMode = mode
		m.statusBar.SetPromptMode(string(mode))
		return nil
	}
	return fmt.Errorf("invalid prompt mode: %s", mode)
}

// Init sets up initial commands.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.input.Init(),
		m.spinner.Tick,
		m.checkConnection(),
		bannerTick(),
	)
}

// checkConnection verifies server connectivity.
func (m Model) checkConnection() tea.Cmd {
	return func() tea.Msg {
		if err := m.client.HealthCheck(); err != nil {
			return DisconnectedMsg{Err: err}
		}
		return ConnectedMsg{}
	}
}

// sendMessage sends the user's input to the server.
// Returns StreamStartMsg with the event channel on success.
func (m Model) sendMessage(content string) tea.Cmd {
	return func() tea.Msg {
		wd, _ := os.Getwd()
		req := protocol.ChatRequest{
			SessionID:  m.sessionID,
			Message:    content,
			WorkDir:    wd,
			PromptType: m.promptMode,
			Metadata: &protocol.Metadata{
				OS:    "linux",
				Shell: m.cfg.Shell.Shell,
			},
		}

		events, err := m.client.SendMessage(req)
		if err != nil {
			return StreamErrorMsg{Err: err}
		}

		return StreamStartMsg{Events: events}
	}
}

// readNextEvent returns a Cmd that reads the next event from a channel.
func readNextEvent(events <-chan protocol.ServerEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-events
		if !ok {
			return StreamDoneMsg{}
		}
		return ServerEventMsg{Event: evt}
	}
}

// addUserMessage appends a user message to the conversation.
func (m *Model) addUserMessage(content string) {
	m.messages = append(m.messages, protocol.Message{
		ID:        uuid.New().String(),
		Role:      protocol.RoleUser,
		Content:   content,
		CreatedAt: time.Now(),
	})
}

// applyLayout (re)computes the viewport size based on current width/height
// and whether the debug panel is open. Called on WindowSize and whenever
// the debug panel is toggled.
func (m *Model) applyLayout() {
	if m.width == 0 || m.height == 0 {
		return
	}

	m.statusBar.SetWidth(m.width)
	m.chatView.SetWidth(m.width)
	m.input.SetWidth(m.width)

	headerHeight := 1
	inputHeight := 5
	reserved := headerHeight + inputHeight + 2
	if m.debugOpen {
		reserved += m.debugHeight
	}
	contentHeight := m.height - reserved
	if contentHeight < 1 {
		contentHeight = 1
	}

	if !m.ready {
		m.viewport = viewport.New(m.width, contentHeight)
		m.viewport.SetYOffset(0)
		m.ready = true
	} else {
		m.viewport.Width = m.width
		m.viewport.Height = contentHeight
	}
	m.updateViewportContent()
}

// addSystemMessage appends a system-role note (used for slash-command output).
func (m *Model) addSystemMessage(content string) {
	m.messages = append(m.messages, protocol.Message{
		ID:        uuid.New().String(),
		Role:      protocol.RoleSystem,
		Content:   content,
		CreatedAt: time.Now(),
	})
}

// finalizeAssistantMessage converts the streaming buffer into a message.
// The stored content has <think>…</think> blocks collapsed, so re-rendering
// history stays tidy.
func (m *Model) finalizeAssistantMessage() {
	if m.streamContent != "" {
		m.messages = append(m.messages, protocol.Message{
			ID:        uuid.New().String(),
			Role:      protocol.RoleAssistant,
			Content:   collapseThink(m.streamContent),
			ToolCalls: m.toolCalls,
			CreatedAt: time.Now(),
		})
		m.streamContent = ""
		m.toolCalls = nil
	}
}
