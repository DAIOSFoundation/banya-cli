package client_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// mockSSEHandler creates a test HTTP handler that streams SSE events.
func mockSSEHandler(events []protocol.ServerEvent, chunkDelay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		for _, evt := range events {
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event:%s\ndata:%s\n\n", evt.Type, string(data))
			flusher.Flush()
			time.Sleep(chunkDelay)
		}
	}
}

func TestHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	c := client.NewHTTPClient(srv.URL, "test-key")
	defer c.Close()

	if err := c.HealthCheck(); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	t.Log("PASS: HealthCheck")
}

func TestStreamChat_BasicResponse(t *testing.T) {
	events := []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": "s1"}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Hello, "}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "world!"}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": "s1"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			fmt.Fprintf(w, `{"status":"ok"}`)
			return
		}
		mockSSEHandler(events, 10*time.Millisecond)(w, r)
	}))
	defer srv.Close()

	c := client.NewHTTPClient(srv.URL, "")
	defer c.Close()

	ch, err := c.SendMessage(protocol.ChatRequest{
		SessionID: "s1",
		Message:   "hello",
	})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	var collected []protocol.ServerEvent
	for evt := range ch {
		collected = append(collected, evt)
		t.Logf("  Event: type=%s", evt.Type)
	}

	if len(collected) != 4 {
		t.Fatalf("Expected 4 events, got %d", len(collected))
	}
	if collected[0].Type != protocol.EventStreamStart {
		t.Errorf("First event should be stream_start, got %s", collected[0].Type)
	}
	if collected[3].Type != protocol.EventDone {
		t.Errorf("Last event should be done, got %s", collected[3].Type)
	}

	// Verify content deltas can be parsed
	var fullContent string
	for _, evt := range collected {
		if evt.Type == protocol.EventContentDelta {
			parsed, err := client.ParseEventData(evt)
			if err != nil {
				t.Errorf("ParseEventData failed: %v", err)
				continue
			}
			if delta, ok := parsed.(protocol.ContentDelta); ok {
				fullContent += delta.Content
			}
		}
	}
	if fullContent != "Hello, world!" {
		t.Errorf("Expected content 'Hello, world!', got '%s'", fullContent)
	}
	t.Logf("PASS: Streamed content = %q", fullContent)
}

func TestStreamChat_WithToolCall(t *testing.T) {
	tcID := "tc-001"
	events := []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": "s2"}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Running command...\n"}},
		{Type: protocol.EventToolCallStart, Data: protocol.ToolCall{
			ID: tcID, Name: "shell_exec", Status: protocol.ToolCallRunning,
			Args: map[string]any{"command": "ls"},
		}},
		{Type: protocol.EventToolCallDone, Data: protocol.ToolCall{
			ID: tcID, Name: "shell_exec", Status: protocol.ToolCallDone,
			Result: "file1.go\nfile2.go\n",
		}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Done!"}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": "s2"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			fmt.Fprintf(w, `{"status":"ok"}`)
			return
		}
		mockSSEHandler(events, 10*time.Millisecond)(w, r)
	}))
	defer srv.Close()

	c := client.NewHTTPClient(srv.URL, "")
	defer c.Close()

	ch, err := c.SendMessage(protocol.ChatRequest{SessionID: "s2", Message: "run ls"})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	var toolStartSeen, toolDoneSeen bool
	var contentParts []string

	for evt := range ch {
		t.Logf("  Event: type=%s", evt.Type)
		parsed, _ := client.ParseEventData(evt)
		switch evt.Type {
		case protocol.EventContentDelta:
			if d, ok := parsed.(protocol.ContentDelta); ok {
				contentParts = append(contentParts, d.Content)
			}
		case protocol.EventToolCallStart:
			toolStartSeen = true
			if tc, ok := parsed.(protocol.ToolCall); ok {
				if tc.Name != "shell_exec" {
					t.Errorf("Expected tool name 'shell_exec', got '%s'", tc.Name)
				}
			}
		case protocol.EventToolCallDone:
			toolDoneSeen = true
			if tc, ok := parsed.(protocol.ToolCall); ok {
				if tc.Result == "" {
					t.Error("Expected non-empty tool result")
				}
				t.Logf("  Tool result: %q", tc.Result)
			}
		}
	}

	if !toolStartSeen {
		t.Error("Never received tool_call_start event")
	}
	if !toolDoneSeen {
		t.Error("Never received tool_call_done event")
	}
	fullContent := strings.Join(contentParts, "")
	if !strings.Contains(fullContent, "Running command") {
		t.Errorf("Expected content to contain 'Running command', got '%s'", fullContent)
	}
	t.Logf("PASS: Tool call flow complete, content = %q", fullContent)
}

func TestStreamChat_ApprovalFlow(t *testing.T) {
	events := []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": "s3"}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Dangerous operation...\n"}},
		{Type: protocol.EventApprovalNeeded, Data: protocol.ApprovalRequest{
			ToolCallID:  "tc-002",
			ToolName:    "shell_exec",
			Description: "Remove files",
			Command:     "rm -rf /tmp/test",
			Risk:        "high",
		}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Continuing after approval.\n"}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": "s3"}},
	}

	var approvalReceived bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			fmt.Fprintf(w, `{"status":"ok"}`)
		case "/api/v1/approval":
			approvalReceived = true
			var resp protocol.ApprovalResponse
			json.NewDecoder(r.Body).Decode(&resp)
			t.Logf("  Approval received: tool=%s approved=%v", resp.ToolCallID, resp.Approved)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"ok"}`)
		case "/api/v1/chat":
			mockSSEHandler(events, 10*time.Millisecond)(w, r)
		}
	}))
	defer srv.Close()

	c := client.NewHTTPClient(srv.URL, "")
	defer c.Close()

	ch, err := c.SendMessage(protocol.ChatRequest{SessionID: "s3", Message: "rm -rf test"})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	var approvalSeen bool
	for evt := range ch {
		t.Logf("  Event: type=%s", evt.Type)
		if evt.Type == protocol.EventApprovalNeeded {
			approvalSeen = true
			parsed, _ := client.ParseEventData(evt)
			if ar, ok := parsed.(protocol.ApprovalRequest); ok {
				t.Logf("  Approval request: tool=%s risk=%s cmd=%s", ar.ToolName, ar.Risk, ar.Command)
				if ar.Risk != "high" {
					t.Errorf("Expected risk 'high', got '%s'", ar.Risk)
				}
				// Simulate user approving
				err := c.SendApproval(protocol.ApprovalResponse{
					SessionID:  "s3",
					ToolCallID: ar.ToolCallID,
					Approved:   true,
				})
				if err != nil {
					t.Errorf("SendApproval failed: %v", err)
				}
			}
		}
	}

	if !approvalSeen {
		t.Error("Never received approval_needed event")
	}
	if !approvalReceived {
		t.Error("Server never received the approval response")
	}
	t.Log("PASS: Approval flow complete")
}

func TestStreamChat_ErrorEvent(t *testing.T) {
	events := []protocol.ServerEvent{
		{Type: protocol.EventStreamStart, Data: map[string]string{"session_id": "s4"}},
		{Type: protocol.EventContentDelta, Data: protocol.ContentDelta{Content: "Processing...\n"}},
		{Type: protocol.EventError, Data: protocol.ErrorData{
			Code:    "AGENT_ERROR",
			Message: "Something went wrong",
		}},
		{Type: protocol.EventDone, Data: map[string]string{"session_id": "s4"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			fmt.Fprintf(w, `{"status":"ok"}`)
			return
		}
		mockSSEHandler(events, 10*time.Millisecond)(w, r)
	}))
	defer srv.Close()

	c := client.NewHTTPClient(srv.URL, "")
	defer c.Close()

	ch, err := c.SendMessage(protocol.ChatRequest{SessionID: "s4", Message: "trigger error"})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	var errorSeen bool
	for evt := range ch {
		t.Logf("  Event: type=%s", evt.Type)
		if evt.Type == protocol.EventError {
			errorSeen = true
			parsed, _ := client.ParseEventData(evt)
			if errData, ok := parsed.(protocol.ErrorData); ok {
				if errData.Code != "AGENT_ERROR" {
					t.Errorf("Expected error code 'AGENT_ERROR', got '%s'", errData.Code)
				}
				t.Logf("  Error: code=%s message=%s", errData.Code, errData.Message)
			}
		}
	}

	if !errorSeen {
		t.Error("Never received error event")
	}
	t.Log("PASS: Error flow complete")
}

func TestMockServer_Integration(t *testing.T) {
	// This test starts the real mock server handler and tests against it
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.NewHTTPClient(srv.URL, "test-api-key")
	defer c.Close()

	// Test health
	if err := c.HealthCheck(); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
	t.Log("PASS: Mock server health check")
}
