package protocol

// Go mirror of banya-core/packages/shared/src/banya-protocol.ts (RPC envelope).
// When the TS SSOT changes, update this file to match.

// ProtocolVersion is the stdio JSON-RPC protocol version understood by this client.
const ProtocolVersion = "1.0.0"

// RpcMethod names understood by the sidecar.
const (
	MethodChatStart        = "chat.start"
	MethodChatCancel       = "chat.cancel"
	MethodApprovalRespond  = "approval.respond"
	MethodSessionList      = "session.list"
	MethodSessionLoad      = "session.load"
	MethodSessionDelete    = "session.delete"
	MethodProviderList     = "provider.list"
	MethodToolList         = "tool.list"
	MethodMcpList          = "mcp.list"
	MethodPing             = "ping"
)

// RpcRequest is sent from client → sidecar (one per stdin line).
type RpcRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// RpcResponse is the final response for a given request id.
type RpcResponse struct {
	ID     string     `json:"id"`
	Result any        `json:"result,omitempty"`
	Error  *ErrorData `json:"error,omitempty"`
}

// PingResult is the payload returned by the `ping` method.
type PingResult struct {
	Version  string `json:"version"`
	UptimeMs int64  `json:"uptime_ms"`
}

// ChatStartResult is the payload returned by the `chat.start` method
// (before the streaming events begin).
type ChatStartResult struct {
	SessionID string `json:"session_id"`
}

// sidecarLine is the internal envelope used to disambiguate response vs event
// when reading a single line off the sidecar's stdout.
type sidecarLine struct {
	// Response fields
	ID     string     `json:"id,omitempty"`
	Result any        `json:"result,omitempty"`
	Error  *ErrorData `json:"error,omitempty"`
	// Event fields
	Type      EventType `json:"type,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Data      any       `json:"data,omitempty"`
}

// SidecarLine is the public alias exported for other packages that
// need to peek at raw sidecar output.
type SidecarLine = sidecarLine
