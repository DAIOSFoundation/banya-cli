package ui

import (
	"strings"
	"testing"
)

// TestCollapseThink_liveStreaming simulates incremental deltas arriving
// from a reasoning model that starts with "Thinking Process:" and ends
// with </think> before the real answer.
func TestCollapseThink_liveStreaming(t *testing.T) {
	tokens := []string{
		"Thinking", " Process", ":\n\n", "1.", " Step", " one\n",
		"2.", " Step", " two\n",
		"</think>\n\n",
		"안녕하세요, ", "저는 ", "banya ", "입니다.",
	}

	var cumulative strings.Builder
	seenPlainCoT := false
	var finalView string

	for _, tok := range tokens {
		cumulative.WriteString(tok)
		view := collapseThink(cumulative.String())
		finalView = view
		// Before </think> arrives we expect the view to only contain the
		// placeholder — never the raw reasoning text.
		if strings.Contains(view, "Thinking Process") ||
			strings.Contains(view, "Step one") ||
			strings.Contains(view, "Step two") {
			seenPlainCoT = true
			t.Logf("view leaked raw CoT at token %q:\n%s", tok, view)
		}
	}

	if seenPlainCoT {
		t.Fatal("live CoT tokens were rendered in clear before </think>")
	}
	if strings.Contains(finalView, thinkPlaceholder) {
		t.Fatalf("final view still shows placeholder after </think>:\n%s", finalView)
	}
	if !strings.Contains(finalView, "안녕하세요") {
		t.Fatalf("final view missing real answer:\n%s", finalView)
	}
}

func TestCollapseThink_plainResponseNotHidden(t *testing.T) {
	// A non-reasoning response should render as-is.
	tokens := []string{"Hello", " there", "! ", "How", " can", " I ", "help?"}
	var cum strings.Builder
	for _, tok := range tokens {
		cum.WriteString(tok)
	}
	got := collapseThink(cum.String())
	want := "Hello there! How can I help?"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
