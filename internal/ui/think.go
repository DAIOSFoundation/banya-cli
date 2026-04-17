package ui

import "strings"

// thinkPlaceholder is what we show in place of a <think>…</think> block.
// Kept short so it doesn't dominate the reply area.
const thinkPlaceholder = "_(thinking…)_"

// collapseThink rewrites the streaming content so any reasoning-model
// <think>…</think> blocks are replaced with a short placeholder. It also
// handles the "orphan close" case where the opening tag was elided and
// everything up to the first </think> is reasoning.
//
// Called on every render, so partial tags mid-stream (e.g. "<th" at the
// tail of a delta) are tolerated: we trim a trailing prefix that could
// still grow into a tag to avoid flashing the opening angle bracket.
func collapseThink(raw string) string {
	if raw == "" {
		return raw
	}

	// If a </think> appears before any <think>, pretend the stream
	// started inside a think block.
	openIdx := strings.Index(raw, "<think>")
	closeIdx := strings.Index(raw, "</think>")
	if closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx) {
		raw = "<think>" + raw
	}

	var out strings.Builder
	for {
		i := strings.Index(raw, "<think>")
		if i < 0 {
			out.WriteString(raw)
			break
		}
		out.WriteString(raw[:i])
		out.WriteString(thinkPlaceholder)
		raw = raw[i+len("<think>"):]

		j := strings.Index(raw, "</think>")
		if j < 0 {
			// Unclosed — drop the rest; the placeholder already indicates it.
			raw = ""
			break
		}
		raw = raw[j+len("</think>"):]
		// Tidy the newline noise models tend to emit around the closing tag.
		raw = strings.TrimLeft(raw, " \t\r\n")
	}

	result := out.String()

	// Hide a dangling partial tag prefix so the user doesn't briefly see
	// "<th" before the next delta completes it.
	result = trimTagPrefixSuffix(result, "<think>")
	result = trimTagPrefixSuffix(result, "</think>")
	return result
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
