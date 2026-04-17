package client_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cascadecodes/banya-cli/internal/client"
	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// TestChatTraceRaw captures every content_delta token to a file so we
// can see exactly what the llm-server is emitting and whether the
// <think>…</think> tags are where we expect.
func TestChatTraceRaw(t *testing.T) {
	bin := os.Getenv("BANYA_CORE_BIN")
	if bin == "" {
		t.Skip("BANYA_CORE_BIN not set")
	}

	pc, err := client.NewProcessClient(bin)
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	defer pc.Close()
	pc.SetLLMBackend(client.NewLLMServerClient("", "", ""))

	events, err := pc.SendMessage(protocol.ChatRequest{
		Message: "간단히 자기소개 해줘",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var full strings.Builder
	var deltas []string
	timeout := time.After(90 * time.Second)

loop:
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				break loop
			}
			switch evt.Type {
			case protocol.EventContentDelta:
				var s string
				switch d := evt.Data.(type) {
				case protocol.ContentDelta:
					s = d.Content
				case map[string]any:
					s, _ = d["content"].(string)
				}
				if s != "" {
					full.WriteString(s)
					deltas = append(deltas, s)
				}
			case protocol.EventContentDone:
				fmt.Println(">>> got content_done, Data type:", fmt.Sprintf("%T", evt.Data))
				if m, ok := evt.Data.(map[string]any); ok {
					fmt.Printf(">>> content_done full: %q\n", m["content"])
				}
			case protocol.EventDone, protocol.EventError:
				break loop
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}

	raw := full.String()
	fmt.Println("===== RAW CONTENT (len=", len(raw), ") =====")
	fmt.Println(raw)
	fmt.Println("===== FIRST 20 DELTAS =====")
	for i, d := range deltas {
		if i >= 20 {
			break
		}
		fmt.Printf("[%d] %q\n", i, d)
	}
	fmt.Println("===== TAG STATS =====")
	fmt.Println("has <think>:  ", strings.Contains(raw, "<think>"))
	fmt.Println("has </think>: ", strings.Contains(raw, "</think>"))
	fmt.Println("has Thinking Process:", strings.Contains(raw, "Thinking Process"))
}
