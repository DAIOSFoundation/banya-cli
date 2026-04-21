package ui

import (
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/ui/commands"
	"github.com/cascadecodes/banya-cli/internal/ui/components"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// runSlashCommand dispatches a /command line through the registry and
// renders the result as a system message.
func (m Model) runSlashCommand(line string) (tea.Model, tea.Cmd) {
	ctx := commands.Context{
		Client:    m.client,
		Config:    m.cfg,
		SessionID: m.sessionID,
		PromptMode: func() protocol.PromptType {
			return m.promptMode
		},
		SetPromptMode: func(mode protocol.PromptType) error {
			return (&m).SetPromptMode(mode)
		},
		SetLanguage: func(lang string) error {
			return (&m).SetLanguage(lang)
		},
		ApplyLLMPreset: func(id string) error {
			return (&m).ApplyLLMPreset(id)
		},
	}
	res := m.commands.Dispatch(line, ctx)

	m.input.Reset()
	m.lastError = ""

	if res.Clear {
		m.messages = nil
		m.streamContent = ""
		m.toolCalls = nil
		m.showBanner = false
	}
	if res.Output != "" {
		m.addSystemMessage(res.Output)
	}
	m.updateViewportContent()

	if res.Quit {
		return m, tea.Quit
	}
	if res.Cmd != nil {
		return m, res.Cmd
	}
	return m, nil
}

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

		m.applyLayout()

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

	case commands.OpenSettingsMsg:
		// Emitted by the /settings slash command. Seed the form with the
		// current Subagent config so the user can tweak instead of
		// re-entering from scratch.
		sm := components.NewSettingsModel(m.cfg.Subagent, m.cfg.Language, m.cfg.LLMServer)
		m.settings = &sm
		m.state = StateSettings
		m.input.Blur()
		m.lastError = ""
		return m, sm.Init()

	case components.SettingsClosedMsg:
		// Settings form submitted or cancelled. Apply the result to the
		// in-memory cfg (saved to disk by the form itself) and return
		// the input focus.
		if msg.Result.Saved {
			m.cfg.Subagent = msg.Result.Subagent
			if msg.Result.Language != "" {
				m.cfg.Language = msg.Result.Language
			}
			extra := ""
			if msg.Result.LLMPresetID != "" {
				if err := (&m).ApplyLLMPreset(msg.Result.LLMPresetID); err != nil {
					extra = "  ⚠ main model swap failed: " + err.Error()
				} else {
					extra = "  ✓ main model → " + msg.Result.LLMPresetID
				}
			}
			m.addSystemMessage("설정 저장됨 (언어: " + m.cfg.Language + ")." + extra + " Subagent 변경은 sidecar 재시작 시 적용.")
		} else if msg.Result.Err != nil {
			m.lastError = "settings save failed: " + msg.Result.Err.Error()
		} else {
			m.addSystemMessage("설정 취소됨.")
		}
		m.settings = nil
		m.state = StateReady
		cmds = append(cmds, m.input.Focus())
		m.updateViewportContent()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		m.toolView.SetSpinner(m.spinner)
		cmds = append(cmds, cmd)
		if m.state == StateStreaming {
			m.updateViewportContent()
		}

	case thinkTickMsg:
		m.thinkingFrame++
		if m.state == StateStreaming {
			cmds = append(cmds, thinkTick())
		}
		m.updateViewportContent()

	case creativeTickMsg:
		// Advance the ticker and schedule the next tick only while we're
		// still streaming. On state transition back to Ready the ticker
		// naturally halts — no explicit stop needed.
		if m.state == StateStreaming && m.creativeTicker != nil {
			m.tickerEmoji, m.tickerWord = m.creativeTicker.Next()
			m.creativeTickCount++
			cmds = append(cmds, creativeTick())
			m.updateViewportContent()
			// Periodic belt-and-suspenders: every ~3 seconds during
			// streaming, ask Bubble Tea to wipe the screen and redraw
			// from scratch. Works around terminals whose alt-screen
			// diff misses trailing artifacts when content height
			// fluctuates (observed on macOS Terminal with rapid
			// multi-line viewport churn). Cheap in practice — one
			// full repaint ≈ few KB of escape codes.
			if m.creativeTickCount%5 == 0 {
				cmds = append(cmds, tea.ClearScreen)
			}
		}
	}

	// Update sub-components
	if m.state == StateReady {
		var inputCmd tea.Cmd
		m.input, inputCmd = m.input.Update(msg)
		cmds = append(cmds, inputCmd)
	}

	// StateSettings: forward non-key msgs to the form so async cmds
	// (huh internal ticks 등) 이 처리됨.
	if m.state == StateSettings && m.settings != nil {
		if _, isKey := msg.(tea.KeyMsg); !isKey {
			next, cmd := m.settings.Update(msg)
			m.settings = &next
			cmds = append(cmds, cmd)
		}
	}

	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

// handleKeyPress processes keyboard input.
func (m Model) handleKeyPress(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// StateSettings 는 huh form 이 전담 — ctrl+c 만 글로벌로 가로채고 나머지는
	// 통째로 폼에 전달.
	if m.state == StateSettings && m.settings != nil {
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		next, cmd := m.settings.Update(msg)
		m.settings = &next
		return m, cmd
	}

	// Slash-command menu key interception. Only fires in StateReady and
	// only when the input currently has an open menu (starts with '/' and
	// the user hasn't typed a space yet). Tab/Up/Down/Esc are handled
	// here so they don't leak into the textarea.
	if m.state == StateReady {
		menuItems := components.FilterSlashCommands(m.input.Value(), m.commands.All())
		if len(menuItems) > 0 {
			switch msg.String() {
			case "up", "ctrl+p":
				if m.slashSelected > 0 {
					m.slashSelected--
				} else {
					m.slashSelected = len(menuItems) - 1
				}
				return m, nil
			case "down", "ctrl+n":
				if m.slashSelected < len(menuItems)-1 {
					m.slashSelected++
				} else {
					m.slashSelected = 0
				}
				return m, nil
			case "tab":
				if m.slashSelected >= len(menuItems) {
					m.slashSelected = 0
				}
				pick := menuItems[m.slashSelected]
				m.input.SetValue("/" + pick.Name + " ")
				m.slashSelected = 0
				return m, nil
			case "enter":
				// Run the currently selected command with no args. The
				// menu is only visible when there's no space in the
				// input, so any partial name typed so far is replaced
				// by the selection. Commands that need args (e.g. /model
				// <id>) will print a usage hint when invoked with none —
				// that's the right affordance: user can then re-type with
				// the hint visible.
				if m.slashSelected >= len(menuItems) {
					m.slashSelected = 0
				}
				pick := menuItems[m.slashSelected]
				m.input.Reset()
				m.slashSelected = 0
				return m.runSlashCommand("/" + pick.Name)
			case "esc":
				// Clear only the leading '/' partial so the menu hides
				// without nuking any arg the user might have typed — but
				// since the menu is only open when there's no space yet,
				// we can safely reset the whole input.
				m.input.Reset()
				m.slashSelected = 0
				return m, nil
			}
			// Clamp selection if the user's keystroke shrank the match
			// set (typed a letter that narrowed the filter).
			if m.slashSelected >= len(menuItems) {
				m.slashSelected = len(menuItems) - 1
			}
		} else {
			m.slashSelected = 0
		}
	}

	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit

	case "ctrl+t":
		m.debugOpen = !m.debugOpen
		m.applyLayout()
		return m, nil

	case "enter":
		if m.state == StateReady {
			content := m.input.Value()
			if content == "" {
				return m, nil
			}
			if commands.IsSlashCommand(content) {
				return m.runSlashCommand(content)
			}

			m.showBanner = false
			m.addUserMessage(content)
			m.input.Reset()
			m.input.Blur()
			m.state = StateStreaming
			m.streamContent = ""
			m.toolCalls = nil
			m.lastError = ""
			m.thinkingFrame = 0
			// Seed the creative ticker so the animation has something
			// to paint on the first render before the first tick fires.
			if m.creativeTicker != nil {
				m.tickerEmoji, m.tickerWord = m.creativeTicker.Next()
			}
			m.debugBuf.push("USER", content)
			m.updateViewportContent()
			return m, tea.Batch(m.sendMessage(content), thinkTick(), creativeTick())
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
			m.debugBuf.push("TOK", delta.Content)
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
			m.debugBuf.push("ERR", errData.Code+": "+errData.Message)
		}

	case protocol.EventDone:
		m.debugBuf.push("EVT", "done")
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

	// Creative ticker sits between the last message and the streaming
	// response — "사용자 명령 바로 아래" per the original UX ask. The
	// line is padded to m.width and a trailing newline added so the
	// diff-based terminal renderer fully overwrites any wider previous
	// frame, eliminating character ghosts when emoji widths differ.
	if m.state == StateStreaming && m.tickerEmoji != "" && m.tickerWord != "" {
		content += "\n" + padRight(renderCreativeTicker(m.tickerEmoji, m.tickerWord), m.width) + "\n"
	}

	if m.streamContent != "" {
		content += m.chatView.RenderStreamingContent(
			collapseThinkWithPlaceholder(m.streamContent, thinkingAnimationFrame(m.thinkingFrame)),
		)
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
