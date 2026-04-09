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

	// Close cleans up resources.
	Close() error
}
