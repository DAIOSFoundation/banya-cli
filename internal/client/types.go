package client

import "github.com/cascadecodes/banya-cli/pkg/protocol"

// Client defines the interface for communicating with the server-side code agent API.
type Client interface {
	// SendMessage sends a chat request and returns a channel of server events (SSE stream).
	SendMessage(req protocol.ChatRequest) (<-chan protocol.ServerEvent, error)

	// SendApproval responds to an approval request from the server.
	SendApproval(resp protocol.ApprovalResponse) error

	// HealthCheck verifies connectivity to the server.
	HealthCheck() error

	// ListSessions returns the saved conversations the sidecar knows
	// about. Empty slice is a valid response (no sessions yet); an
	// error signals transport / sidecar failure. Session persistence
	// lives ENTIRELY on the sidecar side now — banya-cli is a thin
	// client here, no local mirror.
	ListSessions() ([]protocol.SessionSummary, error)

	// LoadSession retrieves the conversation history for the given
	// session id. Returns an empty slice when the id isn't known;
	// error signals RPC failure.
	LoadSession(id string) ([]protocol.Message, error)

	// DeleteSession removes a saved conversation. `deleted` is false
	// when the id wasn't present on the sidecar.
	DeleteSession(id string) (bool, error)

	// Close cleans up resources.
	Close() error
}
