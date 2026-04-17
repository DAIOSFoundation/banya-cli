package client_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// TestChatEndToEnd drives a full chat turn through the sidecar:
//
//	cli → chat.start → sidecar → llm.chat (host callback) → LLMServerClient
//	→ llm-server :8083 → streaming tokens → TUI-ready ServerEvents.
//
// Gated on BANYA_CORE_BIN to keep `go test ./...` fast.
func TestChatEndToEnd(t *testing.T) {
	bin := os.Getenv("BANYA_CORE_BIN")
	if bin == "" {
		t.Skip("BANYA_CORE_BIN not set")
	}

	pc, err := client.NewProcessClient(bin)
	if err != nil {
		t.Fatalf("resolve sidecar: %v", err)
	}
	defer pc.Close()

	pc.SetLLMBackend(client.NewLLMServerClient("", "", ""))

	events, err := pc.SendMessage(protocol.ChatRequest{
		Message: "한 문장으로 자기소개 해줘.",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var got strings.Builder
	deadline := time.After(60 * time.Second)
	sawStart, sawDone := false, false

readloop:
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				break readloop
			}
			switch evt.Type {
			case protocol.EventStreamStart:
				sawStart = true
			case protocol.EventContentDelta:
				if m, ok := evt.Data.(map[string]any); ok {
					if s, _ := m["content"].(string); s != "" {
						got.WriteString(s)
					}
				} else if cd, ok := evt.Data.(protocol.ContentDelta); ok {
					got.WriteString(cd.Content)
				}
			case protocol.EventContentDone:
				// final content already captured from deltas
			case protocol.EventDone:
				sawDone = true
				break readloop
			case protocol.EventError:
				t.Fatalf("error event: %+v", evt.Data)
			}
		case <-deadline:
			t.Fatalf("timeout after 60s (got %d bytes, start=%v done=%v)", got.Len(), sawStart, sawDone)
		}
	}

	if !sawStart {
		t.Fatal("never received stream_start")
	}
	if !sawDone {
		t.Fatal("never received done")
	}
	if got.Len() == 0 {
		t.Fatal("no content received from llm-server")
	}
	t.Logf("assistant (%d bytes):\n%s", got.Len(), got.String())
}
