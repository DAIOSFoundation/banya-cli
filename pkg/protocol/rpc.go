package protocol

import "encoding/json"

// Go mirror of banya-core/packages/shared/src/banya-protocol.ts (RPC envelope).
// When the TS SSOT changes, update this file to match.

// ProtocolVersion is the stdio JSON-RPC protocol version understood by this client.
const ProtocolVersion = "1.0.0"

// Sidecar-owned method names (client → sidecar).
const (
	MethodChatStart       = "chat.start"
	MethodChatCancel      = "chat.cancel"
	MethodApprovalRespond = "approval.respond"
	MethodSessionList     = "session.list"
	MethodSessionLoad     = "session.load"
	MethodSessionDelete   = "session.delete"
	MethodProviderList    = "provider.list"
	MethodToolList        = "tool.list"
	MethodMcpList         = "mcp.list"
	MethodPing            = "ping"
)

// Host-owned method names (sidecar → client). The host (cli) is responsible
// for fulfilling these — notably, all LLM traffic flows through the host so
// the sidecar never talks to llm-server directly.
const (
	MethodLlmChat   = "llm.chat"
	MethodLlmCancel = "llm.cancel"
)

// LlmChatParams is the payload the sidecar sends on `llm.chat`.
type LlmChatParams struct {
	Messages    []LlmChatMessage `json:"messages"`
	Model       string           `json:"model,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	TopP        float64          `json:"top_p,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
}

// LlmChatMessage is a single message in an `llm.chat` request.
type LlmChatMessage struct {
	Role    MessageRole `json:"role"`
	Content string      `json:"content"`
}

// LlmChatResult is the final response the host returns for `llm.chat`.
type LlmChatResult struct {
	Content      string `json:"content"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// LlmCancelParams tells the host to cancel an in-flight llm.chat.
type LlmCancelParams struct {
	RequestID string `json:"request_id"`
}

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

// SidecarLine is the on-wire envelope used to disambiguate request vs
// response vs event when reading a single line off the sidecar's stdout.
// Exactly one of (method), (result|error), (type) is populated per line.
type SidecarLine struct {
	// Common id (request or response correlation).
	ID string `json:"id,omitempty"`

	// Request fields (sidecar → host host-callable RPC).
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`

	// Response fields (sidecar → host reply).
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorData      `json:"error,omitempty"`

	// Event fields (sidecar → host streaming updates).
	Type      EventType `json:"type,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Data      any       `json:"data,omitempty"`
}
