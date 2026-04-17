// Package protocol defines the message types exchanged between
// the banya CLI client and the server-side code agent API.
package protocol

import "time"

// MessageRole identifies who authored a message.
type MessageRole string

const (
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleSystem    MessageRole = "system"
	RoleTool      MessageRole = "tool"
)

// EventType identifies the kind of SSE event the server sends.
type EventType string

const (
	EventStreamStart    EventType = "stream_start"
	EventContentDelta   EventType = "content_delta"
	EventContentDone    EventType = "content_done"
	EventToolCallStart  EventType = "tool_call_start"
	EventToolCallDelta  EventType = "tool_call_delta"
	EventToolCallDone   EventType = "tool_call_done"
	EventApprovalNeeded EventType = "approval_needed"
	EventError          EventType = "error"
	EventDone           EventType = "done"
)

// ToolCallStatus tracks the lifecycle of a single tool invocation.
type ToolCallStatus string

const (
	ToolCallPending  ToolCallStatus = "pending"
	ToolCallRunning  ToolCallStatus = "running"
	ToolCallApproval ToolCallStatus = "approval"
	ToolCallDone     ToolCallStatus = "done"
	ToolCallFailed   ToolCallStatus = "failed"
)

// --- Request types ---

// PromptType selects the system prompt Core will compose for the turn.
// Short names: "code" | "ask" | "plan" | "agent".
type PromptType string

const (
	PromptCode  PromptType = "code"
	PromptAsk   PromptType = "ask"
	PromptPlan  PromptType = "plan"
	PromptAgent PromptType = "agent"
)

// ChatRequest is the payload sent by the CLI to start or continue a conversation.
type ChatRequest struct {
	SessionID  string     `json:"session_id,omitempty"`
	Message    string     `json:"message"`
	Files      []string   `json:"files,omitempty"`
	WorkDir    string     `json:"work_dir,omitempty"`
	PromptType PromptType `json:"prompt_type,omitempty"`
	Metadata   *Metadata  `json:"metadata,omitempty"`
}

// ApprovalResponse is sent when the user approves or denies a tool call.
type ApprovalResponse struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Approved   bool   `json:"approved"`
}

// Metadata carries environment context for the server agent.
type Metadata struct {
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Shell    string `json:"shell,omitempty"`
	Language string `json:"language,omitempty"`
}

// --- SSE event types ---

// ServerEvent is the envelope for every SSE message from the server.
type ServerEvent struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"session_id,omitempty"`
	Data      any       `json:"data,omitempty"`
}

// ContentDelta carries an incremental text chunk.
type ContentDelta struct {
	Content string `json:"content"`
}

// ToolCall describes a tool invocation requested or executed by the agent.
type ToolCall struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Args      map[string]any    `json:"args,omitempty"`
	Status    ToolCallStatus    `json:"status"`
	Result    string            `json:"result,omitempty"`
	Error     string            `json:"error,omitempty"`
	StartedAt time.Time         `json:"started_at,omitempty"`
	EndedAt   time.Time         `json:"ended_at,omitempty"`
}

// ApprovalRequest is sent when the server needs user confirmation before executing.
type ApprovalRequest struct {
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	Risk        string `json:"risk,omitempty"` // low, medium, high
}

// ErrorData carries error information from the server.
type ErrorData struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Local message model ---

// Message is the local representation of a conversation turn, used for
// rendering in the TUI and persisting to local history.
type Message struct {
	ID        string      `json:"id"`
	Role      MessageRole `json:"role"`
	Content   string      `json:"content"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}

// FileChange describes a file modification performed by the agent.
type FileChange struct {
	Path      string `json:"path"`
	Action    string `json:"action"` // create, modify, delete
	Diff      string `json:"diff,omitempty"`
	Language  string `json:"language,omitempty"`
}
