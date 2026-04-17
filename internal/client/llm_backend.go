package client

import (
	"context"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// LLMBackend is any provider that can fulfill an `llm.chat` host call.
// The cli owns the connection; the banya-core sidecar never talks to
// llm-server directly. When a sidecar is in use, the ProcessClient
// forwards inbound `llm.chat` requests to this backend.
type LLMBackend interface {
	// Chat runs a (non-streaming or streaming) chat completion. Tokens
	// are delivered through onToken as they arrive. Returns the full
	// content, finish reason, and any native tool calls the model emitted.
	Chat(ctx context.Context, params protocol.LlmChatParams, onToken func(string) error) (content, finishReason string, toolCalls []protocol.LlmToolCall, err error)

	// Close releases any underlying transport resources.
	Close() error
}
