package ui

import "testing"

func TestCollapseThink_pairedTags(t *testing.T) {
	got := collapseThink("<think>reasoning here</think>\n\nFinal answer")
	want := "Final answer"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCollapseThink_orphanClose(t *testing.T) {
	got := collapseThink("Thinking Process...\nstep 1\n</think>\n\nFinal answer")
	want := "Final answer"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCollapseThink_unclosed(t *testing.T) {
	got := collapseThink("Answer begins <think>still reasoning")
	want := "Answer begins\n\n" + thinkPlaceholder
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCollapseThink_partialTagSuffix(t *testing.T) {
	// Delta stream ends mid-tag; we should hide the partial prefix.
	for _, partial := range []string{"<", "<t", "<th", "<thi", "<thin", "<think"} {
		got := collapseThink("Before " + partial)
		if got != "Before" {
			t.Fatalf("partial=%q got %q, want %q", partial, got, "Before")
		}
	}
}

func TestCollapseThink_multipleBlocks(t *testing.T) {
	got := collapseThink("<think>a</think>foo<think>b</think>bar")
	want := "foobar"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCollapseThink_noTags(t *testing.T) {
	got := collapseThink("just a normal answer")
	if got != "just a normal answer" {
		t.Fatalf("got %q", got)
	}
}

func TestCollapseThink_empty(t *testing.T) {
	if got := collapseThink(""); got != "" {
		t.Fatalf("got %q", got)
	}
}
