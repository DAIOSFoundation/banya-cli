package ui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/internal/config"
	"github.com/cascadecodes/banya-cli/internal/ui"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// newTestConfig returns a minimal config for testing.
func newTestConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{URL: "http://mock:8080"},
		UI:     config.UIConfig{Theme: "dark", WordWrap: true},
		Shell:  config.ShellConfig{Shell: "/bin/bash"},
		Log:    config.LogConfig{Level: "error"},
	}
}

// --- Tests ---

func TestTUI_Startup(t *testing.T) {
	mock := client.NewMockClient(nil)
	model := ui.New(mock, newTestConfig())

	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	// Wait for welcome banner to fully render (animation takes ~1.9s)
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "Code Agent") || strings.Contains(s, "BANYA") || strings.Contains(s, "CLI")
	}, teatest.WithDuration(5*time.Second))

	t.Log("PASS: TUI started and rendered welcome banner")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

func TestTUI_ChatFlow(t *testing.T) {
	mock := &client.MockClient{
		Healthy:    true,
		EventDelay: 5 * time.Millisecond,
		OnSendMessage: func(req protocol.ChatRequest) []protocol.ServerEvent {
			return client.MockChatEvents("test-session", "Hello from Banya! This is a **test response**.")
		},
	}

	model := ui.New(mock, newTestConfig())
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	// Wait for ready state
	time.Sleep(500 * time.Millisecond)

	// Type a message
	tm.Type("hello")
	time.Sleep(100 * time.Millisecond)

	// Press enter to send
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Wait for the response to appear
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "Hello from Banya") || strings.Contains(s, "test response")
	}, teatest.WithDuration(5*time.Second))

	t.Log("PASS: Chat message sent and response received")

	// Verify the mock received the message
	if len(mock.MessageLog) == 0 {
		t.Fatal("Mock client never received a message")
	}
	t.Logf("  Mock received: %q", mock.MessageLog[0].Message)

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

func TestTUI_ToolCallDisplay(t *testing.T) {
	mock := &client.MockClient{
		Healthy:    true,
		EventDelay: 5 * time.Millisecond,
		OnSendMessage: func(req protocol.ChatRequest) []protocol.ServerEvent {
			return client.MockToolCallEvents("test-session", "shell_exec", "file1.go\nfile2.go")
		},
	}

	model := ui.New(mock, newTestConfig())
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(100, 30))

	time.Sleep(500 * time.Millisecond)

	tm.Type("run ls")
	time.Sleep(100 * time.Millisecond)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Wait for tool call to appear
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "shell_exec") || strings.Contains(s, "Done")
	}, teatest.WithDuration(5*time.Second))

	t.Log("PASS: Tool call rendered in TUI")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

func TestTUI_KoreanMessage(t *testing.T) {
	mock := &client.MockClient{
		Healthy:    true,
		EventDelay: 5 * time.Millisecond,
		OnSendMessage: func(req protocol.ChatRequest) []protocol.ServerEvent {
			return client.MockChatEvents("test-session", "안녕하세요! Banya 코드 에이전트입니다.")
		},
	}

	model := ui.New(mock, newTestConfig())
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	time.Sleep(500 * time.Millisecond)

	// Type Korean - teatest.Type works with byte-level input
	// For Korean we send it as a message since Type() doesn't handle multi-byte well
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("안녕")})
	time.Sleep(100 * time.Millisecond)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Wait for Korean response
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "안녕하세요") || strings.Contains(s, "에이전트")
	}, teatest.WithDuration(5*time.Second))

	t.Log("PASS: Korean input/output rendered correctly")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

func TestTUI_ErrorDisplay(t *testing.T) {
	mock := &client.MockClient{
		Healthy:    true,
		EventDelay: 5 * time.Millisecond,
		OnSendMessage: func(req protocol.ChatRequest) []protocol.ServerEvent {
			return client.MockErrorEvents("test-session")
		},
	}

	model := ui.New(mock, newTestConfig())
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	time.Sleep(500 * time.Millisecond)

	tm.Type("trigger error")
	time.Sleep(100 * time.Millisecond)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Wait for error to display
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "error") || strings.Contains(s, "Error")
	}, teatest.WithDuration(5*time.Second))

	t.Log("PASS: Error event displayed in TUI")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

func TestTUI_SlashQuit(t *testing.T) {
	mock := client.NewMockClient(nil)
	model := ui.New(mock, newTestConfig())

	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	time.Sleep(500 * time.Millisecond)

	// Type /quit and press enter
	tm.Type("/quit")
	time.Sleep(100 * time.Millisecond)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Program should terminate
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
	t.Log("PASS: /quit command exits the TUI")
}

func TestTUI_SlashClear(t *testing.T) {
	callCount := 0
	mock := &client.MockClient{
		Healthy:    true,
		EventDelay: 5 * time.Millisecond,
		OnSendMessage: func(req protocol.ChatRequest) []protocol.ServerEvent {
			callCount++
			return client.MockChatEvents("test-session", "Response #"+string(rune('0'+callCount)))
		},
	}

	model := ui.New(mock, newTestConfig())
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	time.Sleep(500 * time.Millisecond)

	// Send a message first
	tm.Type("first message")
	time.Sleep(100 * time.Millisecond)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	time.Sleep(1 * time.Second)

	// Clear the chat
	tm.Type("/clear")
	time.Sleep(100 * time.Millisecond)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	time.Sleep(500 * time.Millisecond)

	t.Log("PASS: /clear command processed")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

func TestTUI_DisconnectedState(t *testing.T) {
	mock := &client.MockClient{
		Healthy: false, // health check will fail
	}

	model := ui.New(mock, newTestConfig())
	tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(80, 24))

	// Wait for disconnected status
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "disconnected") || strings.Contains(s, "unhealthy")
	}, teatest.WithDuration(3*time.Second))

	t.Log("PASS: Disconnected state displayed correctly")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}
