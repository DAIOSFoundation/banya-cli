package ui

import "strings"

// thinkPlaceholder is the static fallback used when no animation frame
// is supplied (tests, final message storage).
const thinkPlaceholder = "_(thinking…)_"

// thinkingAnimationFrame returns a "thinking", "thinking.", "thinking..",
// "thinking..." style marker for the given frame counter.
func thinkingAnimationFrame(frame int) string {
	dots := strings.Repeat(".", frame%4)
	return "_thinking" + dots + "_"
}

// Known implicit "start-of-reasoning" markers. Some models (notably the
// Gemini family) emit their chain-of-thought without an opening <think>
// tag but eventually close it with </think>. If a stream starts with one
// of these phrases, we treat it as if it had begun inside a think block.
var implicitThinkIntros = []string{
	"<think>",
	"Thinking Process:",
	"Thinking Process\n",
	"Thinking:\n",
	"Thinking:",
	"[Thinking]",
	"Let me think",
}

// collapseThink rewrites the streaming content so any reasoning-model
// <think>…</think> blocks are replaced with a short placeholder.
//
// It handles three real-world shapes:
//  1. paired <think>...</think>
//  2. orphan </think> (open tag elided; everything up to </think> is reasoning)
//  3. open-ended CoT streaming: no tags yet, but the content begins with a
//     known reasoning intro like "Thinking Process:" — we hide it live and
//     wait for </think> (or stream end) to resume normal rendering.
//
// Called on every render, so partial tags mid-stream (e.g. "<th" at the
// tail of a delta) are tolerated: we trim a trailing prefix that could
// still grow into a tag to avoid flashing the opening angle bracket.
func collapseThink(raw string) string {
	return collapseThinkWithPlaceholder(raw, thinkPlaceholder)
}

// collapseThinkWithPlaceholder is collapseThink with a caller-supplied
// placeholder (e.g. an animated "thinking..." frame).
//
// Rendering rules:
//   - while inside an open think block (CoT still streaming): show the
//     placeholder ONLY — no real content is displayed yet
//   - once the think block is closed and real content follows: show the
//     real content ONLY — the placeholder is dropped entirely
//   - mixed case (real text before an unclosed think): real text plus a
//     trailing placeholder to signal ongoing reasoning
func collapseThinkWithPlaceholder(raw string, placeholder string) string {
	if raw == "" {
		return raw
	}

	// If a </think> appears before any <think>, pretend the stream
	// started inside a think block.
	openIdx := strings.Index(raw, "<think>")
	closeIdx := strings.Index(raw, "</think>")
	if closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx) {
		raw = "<think>" + raw
	} else if openIdx < 0 && hasImplicitThinkIntro(raw) {
		raw = "<think>" + raw
	}

	var real strings.Builder
	insideUnclosed := false

	for {
		i := strings.Index(raw, "<think>")
		if i < 0 {
			real.WriteString(raw)
			break
		}
		real.WriteString(raw[:i])
		raw = raw[i+len("<think>"):]

		j := strings.Index(raw, "</think>")
		if j < 0 {
			// Still inside — no more real content after this.
			insideUnclosed = true
			raw = ""
			break
		}
		raw = raw[j+len("</think>"):]
		raw = strings.TrimLeft(raw, " \t\r\n")
	}

	result := real.String()
	result = trimTagPrefixSuffix(result, "<think>")
	result = trimTagPrefixSuffix(result, "</think>")
	result = strings.TrimSpace(result)

	if insideUnclosed {
		if result == "" {
			return placeholder
		}
		return result + "\n\n" + placeholder
	}
	// Fully past any think blocks — return just the real content.
	return result
}

// hasImplicitThinkIntro reports whether raw looks like (or is growing
// into) one of the known reasoning-intro phrases. We match both:
//
//   - full prefix match: "Thinking Process:..." matches marker "Thinking Process:"
//   - in-progress prefix: content "Thinking" is a prefix of marker "Thinking Process:"
//
// The in-progress case is important for streaming: we want to hide the
// CoT from the very first delta, not wait until the whole intro phrase
// arrives. A false positive only causes a ~1-char hide-then-reveal
// flicker for non-CoT responses starting with the same letters.
func hasImplicitThinkIntro(raw string) bool {
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	if trimmed == "" {
		return false
	}
	for _, marker := range implicitThinkIntros {
		if len(trimmed) >= len(marker) {
			if strings.HasPrefix(trimmed, marker) {
				return true
			}
			continue
		}
		// trimmed is shorter than marker — check whether trimmed is a
		// leading prefix of marker (stream still growing into it).
		if strings.HasPrefix(marker, trimmed) {
			return true
		}
	}
	return false
}

// trimTagPrefixSuffix removes a trailing proper prefix of tag from s, if any.
func trimTagPrefixSuffix(s, tag string) string {
	for n := len(tag) - 1; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return s[:len(s)-n]
		}
	}
	return s
}
