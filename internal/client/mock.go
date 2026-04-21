package client

import (
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/google/uuid"
)

// MockClient is a test double that implements the Client interface.
// It sends predefined events through a channel when SendMessage is called.
type MockClient struct {
	Healthy       bool
	Events        []protocol.ServerEvent // events to stream for the next SendMessage call
	EventDelay    time.Duration          // delay between events
	ApprovalLog   []protocol.ApprovalResponse
	MessageLog    []protocol.ChatRequest
	OnSendMessage func(req protocol.ChatRequest) []protocol.ServerEvent // dynamic response generator
}

// NewMockClient creates a mock client that passes health checks and returns canned events.
func NewMockClient(events []protocol.ServerEvent) *MockClient {
	return &MockClient{
		Healthy:    true,
		Events:     events,
		EventDelay: 5 * time.Millisecond,
	}
}

func (m *MockClient) HealthCheck() error {
	if !m.Healthy {
		return errUnhealthy
	}
	return nil
}

func (m *MockClient) SendMessage(req protocol.ChatRequest) (<-chan protocol.ServerEvent, error) {
	m.MessageLog = append(m.MessageLog, req)

	events := m.Events
	if m.OnSendMessage != nil {
		events = m.OnSendMessage(req)
	}

	ch := make(chan protocol.ServerEvent, len(events)+1)
	go func() {
		defer close(ch)
		for _, evt := range events {
			if m.EventDelay > 0 {
				time.Sleep(m.EventDelay)
			}
			ch <- evt
		}
	}()
	return ch, nil
}

func (m *MockClient) SendApproval(resp protocol.ApprovalResponse) error {
	m.ApprovalLog = append(m.ApprovalLog, resp)
	return nil
}

func (m *MockClient) Close() error {
	return nil
}

// Session RPC stubs for tests — return empty lists so Model startup
// resume paths don't error out in teatest harnesses.
func (m *MockClient) ListSessions() ([]protocol.SessionSummary, error) {
	return nil, nil
}

func (m *MockClient) LoadSession(id string) ([]protocol.Message, error) {
	return nil, nil
}

func (m *MockClient) DeleteSession(id string) (bool, error) {
	return false, nil
}

var errUnhealthy = &unhealthyError{}

type unhealthyError struct{}

func (e *unhealthyError) Error() string { return "mock: server unhealthy" }

// --- Helpers to build common event sequences ---

// MockChatEvents builds a simple text response event sequence.
func MockChatEvents(sessionID, content string) []protocol.ServerEvent {
	return []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": sessionID}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: content}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": sessionID}},
	}
}

// MockToolCallEvents builds a tool call event sequence.
func MockToolCallEvents(sessionID, toolName, result string) []protocol.ServerEvent {
	tcID := uuid.New().String()
	return []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": sessionID}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Executing...\n"}},
		{Type: protocol.EventToolCallStart, Data: protocol.ToolCall{
			ID: tcID, Name: toolName, Status: protocol.ToolCallRunning,
			Args: map[string]any{"command": "test"},
		}},
		{Type: protocol.EventToolCallDone, Data: protocol.ToolCall{
			ID: tcID, Name: toolName, Status: protocol.ToolCallDone, Result: result,
		}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Done.\n"}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": sessionID}},
	}
}

// MockApprovalEvents builds an approval-required event sequence.
func MockApprovalEvents(sessionID string) []protocol.ServerEvent {
	tcID := uuid.New().String()
	return []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": sessionID}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Dangerous operation.\n"}},
		{Type: protocol.EventApprovalNeeded, Data: protocol.ApprovalRequest{
			ToolCallID:  tcID,
			ToolName:    "shell_exec",
			Description: "Remove files",
			Command:     "rm -rf /tmp/test",
			Risk:        "high",
		}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Approved.\n"}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": sessionID}},
	}
}

// MockErrorEvents builds an error event sequence.
func MockErrorEvents(sessionID string) []protocol.ServerEvent {
	return []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": sessionID}},
		{Type: protocol.EventError, Data: protocol.ErrorData{Code: "TEST_ERR", Message: "test error occurred"}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": sessionID}},
	}
}
