package ui

import (
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// Update handles all messages and key events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.statusBar.SetWidth(msg.Width)
		m.chatView.SetWidth(msg.Width)
		m.input.SetWidth(msg.Width)

		headerHeight := 1
		inputHeight := 5
		contentHeight := m.height - headerHeight - inputHeight - 2

		if !m.ready {
			m.viewport = viewport.New(msg.Width, contentHeight)
			m.viewport.SetYOffset(0)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = contentHeight
		}
		m.updateViewportContent()

	case tea.KeyMsg:
		return m.handleKeyPress(msg)

	case bannerTickMsg:
		if m.showBanner && m.bannerLines < totalBannerLines() {
			m.bannerLines++
			if m.bannerLines >= totalBannerLines() {
				// All lines drawn — start tagline typewriter
				m.taglineChars = 0
				cmds = append(cmds, taglineTick())
			} else {
				cmds = append(cmds, bannerTick())
			}
		}

	case taglineTickMsg:
		if m.showBanner && m.taglineChars >= 0 && m.taglineChars < totalTaglineChars() {
			m.taglineChars++
			cmds = append(cmds, taglineTick())
		}

	case ConnectedMsg:
		m.statusBar.SetConnected(true)
		m.statusBar.SetSession(m.sessionID)
		m.lastError = ""

	case DisconnectedMsg:
		m.statusBar.SetConnected(false)
		m.lastError = msg.Err.Error()

	case StreamStartMsg:
		// Store the event channel and start reading
		m.eventChan = msg.Events
		cmds = append(cmds, readNextEvent(m.eventChan))

	case ServerEventMsg:
		return m.handleServerEvent(msg.Event)

	case StreamDoneMsg:
		m.finalizeAssistantMessage()
		m.state = StateReady
		m.eventChan = nil
		m.lastError = ""
		cmds = append(cmds, m.input.Focus())
		m.updateViewportContent()

	case StreamErrorMsg:
		m.lastError = msg.Err.Error()
		m.state = StateReady
		m.eventChan = nil
		cmds = append(cmds, m.input.Focus())

	case ApprovalResultMsg:
		return m.handleApprovalResult(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.toolView.SetSpinner(m.spinner)
		cmds = append(cmds, cmd)
		if m.state == StateStreaming {
			m.updateViewportContent()
		}
	}

	// Update sub-components
	if m.state == StateReady {
		var inputCmd tea.Cmd
		m.input, inputCmd = m.input.Update(msg)
		cmds = append(cmds, inputCmd)
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

// handleKeyPress processes keyboard input.
func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit

	case "enter":
		if m.state == StateReady {
			content := m.input.Value()
			if content == "" {
				return m, nil
			}
			switch content {
			case "/quit", "/exit":
				return m, tea.Quit
			case "/clear":
				m.messages = nil
				m.streamContent = ""
				m.toolCalls = nil
				m.input.Reset()
				m.lastError = ""
				m.updateViewportContent()
				return m, nil
			}

			m.showBanner = false
			m.addUserMessage(content)
			m.input.Reset()
			m.input.Blur()
			m.state = StateStreaming
			m.streamContent = ""
			m.toolCalls = nil
			m.lastError = ""
			m.updateViewportContent()
			return m, m.sendMessage(content)
		}
		if m.state == StateApproval && m.pendingApproval != nil {
			toolCallID := m.pendingApproval.ToolCallID
			return m, func() tea.Msg {
				return ApprovalResultMsg{ToolCallID: toolCallID, Approved: true}
			}
		}

	case "esc":
		if m.state == StateApproval && m.pendingApproval != nil {
			toolCallID := m.pendingApproval.ToolCallID
			return m, func() tea.Msg {
				return ApprovalResultMsg{ToolCallID: toolCallID, Approved: false}
			}
		}
	}

	// Forward to input
	if m.state == StateReady {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	return m, nil
}

// handleServerEvent processes a single SSE event.
func (m Model) handleServerEvent(evt protocol.ServerEvent) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	parsed, _ := client.ParseEventData(evt)

	switch evt.Type {
	case protocol.EventContentDelta:
		if delta, ok := parsed.(protocol.ContentDelta); ok {
			m.streamContent += delta.Content
			m.updateViewportContent()
		}

	case protocol.EventToolCallStart:
		if tc, ok := parsed.(protocol.ToolCall); ok {
			tc.Status = protocol.ToolCallRunning
			m.toolCalls = append(m.toolCalls, tc)
			m.updateViewportContent()
		}

	case protocol.EventToolCallDone:
		if tc, ok := parsed.(protocol.ToolCall); ok {
			for i, existing := range m.toolCalls {
				if existing.ID == tc.ID {
					m.toolCalls[i] = tc
					break
				}
			}
			m.updateViewportContent()
		}

	case protocol.EventApprovalNeeded:
		if ar, ok := parsed.(protocol.ApprovalRequest); ok {
			m.pendingApproval = &ar
			m.state = StateApproval
			m.updateViewportContent()
			// Don't read next event until approval is resolved
			return m, nil
		}

	case protocol.EventError:
		if errData, ok := parsed.(protocol.ErrorData); ok {
			m.lastError = errData.Message
		}

	case protocol.EventDone:
		m.finalizeAssistantMessage()
		m.state = StateReady
		m.eventChan = nil
		cmds = append(cmds, m.input.Focus())
		m.updateViewportContent()
		return m, tea.Batch(cmds...)
	}

	// Continue reading next event from the stream
	if m.eventChan != nil {
		cmds = append(cmds, readNextEvent(m.eventChan))
	}

	return m, tea.Batch(cmds...)
}

// handleApprovalResult sends the approval response to the server.
func (m Model) handleApprovalResult(msg ApprovalResultMsg) (tea.Model, tea.Cmd) {
	m.pendingApproval = nil
	m.state = StateStreaming
	m.updateViewportContent()

	eventChan := m.eventChan
	return m, func() tea.Msg {
		resp := protocol.ApprovalResponse{
			SessionID:  m.sessionID,
			ToolCallID: msg.ToolCallID,
			Approved:   msg.Approved,
		}
		if err := m.client.SendApproval(resp); err != nil {
			return StreamErrorMsg{Err: err}
		}
		// Resume reading from the event stream
		if eventChan != nil {
			evt, ok := <-eventChan
			if !ok {
				return StreamDoneMsg{}
			}
			return ServerEventMsg{Event: evt}
		}
		return StreamDoneMsg{}
	}
}

// updateViewportContent refreshes the viewport with current state.
func (m *Model) updateViewportContent() {
	content := m.chatView.RenderMessages(m.messages)

	if m.streamContent != "" {
		content += m.chatView.RenderStreamingContent(m.streamContent)
	}

	if len(m.toolCalls) > 0 {
		content += "\n" + m.toolView.RenderToolCalls(m.toolCalls)
	}

	if m.pendingApproval != nil {
		content += "\n" + m.renderApprovalPrompt()
	}

	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}
