package ui

import "github.com/cascadecodes/banya-cli/pkg/protocol"

// Bubble Tea message types for the main TUI model.

// StreamStartMsg carries the SSE event channel from the API client.
// This is the first message returned after calling SendMessage.
type StreamStartMsg struct {
	Events <-chan protocol.ServerEvent
}

// ServerEventMsg wraps a server event received from the SSE stream.
type ServerEventMsg struct {
	Event protocol.ServerEvent
}

// StreamDoneMsg signals that the SSE stream has ended.
type StreamDoneMsg struct{}

// StreamErrorMsg signals a stream error.
type StreamErrorMsg struct {
	Err error
}

// ConnectedMsg signals that the server health check passed.
type ConnectedMsg struct{}

// DisconnectedMsg signals that the server is unreachable.
type DisconnectedMsg struct {
	Err error
}

// ApprovalResultMsg carries the user's approval/denial of a tool call.
type ApprovalResultMsg struct {
	ToolCallID string
	Approved   bool
}

