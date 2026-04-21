package ui

import (
	_ "embed"
	"math/rand"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// creativeTickerStyle is the lipgloss style for the single-line
// ticker section rendered between the viewport and the streaming
// state bar. Kept package-level so it isn't rebuilt on every tick.
var creativeTickerStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#FFD166")).
	Italic(true)

// renderCreativeTicker returns the one-line "<emoji>  <word>…" string
// drawn during streaming. The caller is expected to pad the result to
// terminal width — see padRight — so successive frames of different
// widths fully overwrite the previous line.
func renderCreativeTicker(emoji, word string) string {
	return "  " + creativeTickerStyle.Render(emoji+"  "+word+"…")
}

// padRight pads s (lipgloss-measured) to targetWidth with trailing
// spaces. If s is already as wide or wider, it's returned unchanged.
// Used by the creative ticker to guarantee each frame overwrites the
// previous one cleanly — essential when emoji widths differ between
// consecutive picks.
func padRight(s string, targetWidth int) string {
	w := lipgloss.Width(s)
	if w >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-w)
}

//go:embed creative_words.txt
var creativeWordsRaw string

// creativeWordPool is the deduplicated, line-by-line vocabulary that
// drives the streaming ticker. See creative_words.txt for provenance
// and the expansion protocol (new words: just append a line).
var creativeWordPool = loadCreativeWordPool(creativeWordsRaw)

func loadCreativeWordPool(raw string) []string {
	lines := strings.Split(raw, "\n")
	seen := make(map[string]struct{}, len(lines))
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		w := strings.TrimSpace(ln)
		if w == "" || strings.HasPrefix(w, "#") {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

// creativeEmojiPool — curated set spanning expressive ranges (faces,
// animals, plants, food, astronomy, music, arts, symbols, activities).
// Each string may be a single codepoint or a ZWJ sequence; the caller
// treats them as atomic.
var creativeEmojiPool = []string{
	"✨", "💡", "🔮", "🧠", "📚", "📖", "📝", "✏️", "🖋️", "🖌️", "🎨", "🖼️",
	"🎭", "🎬", "🎥", "🎞️", "🎼", "🎵", "🎶", "🎹", "🎸", "🎺", "🎷", "🥁",
	"🎻", "🪕", "🪗", "💃", "🕺", "🤸", "🎪", "🎠", "🎡", "🎢", "🚀", "🛸",
	"🪐", "🌌", "🌠", "⭐", "🌟", "☄️", "🌙", "☀️", "🌞", "🌛", "🌜", "🌈",
	"⚡", "🔥", "💫", "🌀", "🌊", "❄️", "🪐", "🧬", "🧪", "⚗️", "🔬", "🔭",
	"📐", "📏", "📊", "📈", "📉", "🔢", "🧮", "🗝️", "🔑", "🗿", "🏺", "⚱️",
	"🎲", "🎯", "♟️", "🧩", "🪄", "🪬", "🧿", "⚛️", "☯️", "🕉️", "✡️", "☸️",
	"🌸", "🌺", "🌻", "🌹", "🌷", "🌼", "🌱", "🌿", "🍀", "🍃", "🌾", "🌳",
	"🦋", "🐉", "🦄", "🐦", "🦉", "🦜", "🐿️", "🦊", "🦚", "🦩", "🐙", "🐚",
	"🍇", "🍊", "🍋", "🍉", "🍎", "🍏", "🍑", "🍒", "🍓", "🫐", "🥝", "🍍",
	"🧑‍🎨", "🧑‍🔬", "🧑‍🚀", "🧑‍🏫", "🧑‍💻", "🧘", "🧙", "🧚", "🧜", "🧝", "🧞",
	"😀", "😃", "😄", "😁", "😆", "😅", "😂", "🤣", "😊", "🙂", "🙃", "😉",
	"😍", "🥰", "😘", "😗", "😋", "😎", "🤩", "🥳", "🤓", "🧐", "🤔", "🤨",
	"🤯", "😮", "😯", "😲", "🤠", "🤗", "🫡", "🫶", "🤲", "🙏", "👏", "🤝",
	"💎", "🏆", "🥇", "🥈", "🥉", "🎖️", "🏅", "🎗️", "🎁", "🎉", "🎊", "🎈",
}

// creativeTickInterval is the refresh cadence for the ticker. 600ms
// (~1.7 Hz) is slow enough for terminals with weaker alt-screen
// support to keep up without scrollback accumulation, yet still
// gives a lively sense of progress.
const creativeTickInterval = 600 * time.Millisecond

// creativeTickMsg drives the ticker animation. One-shot per tick —
// handler must schedule the next tick itself when it wants to keep
// going (same pattern as thinkTickMsg).
type creativeTickMsg struct{}

// creativeTick returns a Cmd that fires a single creativeTickMsg
// after the standard interval. Issue it from Update after each tick
// (and on streaming start) to keep the animation running.
func creativeTick() tea.Cmd {
	return tea.Tick(creativeTickInterval, func(time.Time) tea.Msg {
		return creativeTickMsg{}
	})
}

// creativeTickerState holds the last-picked word + emoji so the Model
// can avoid repeating either on the very next frame. Seeded lazily on
// first pick so imports don't move test results around.
type creativeTickerState struct {
	rng       *rand.Rand
	lastWord  string
	lastEmoji string
}

// newCreativeTicker returns a fresh ticker with its own RNG. Seeded
// with UnixNano so each session's sequence is distinct; not
// cryptographic — we just want visual variety.
func newCreativeTicker() *creativeTickerState {
	return &creativeTickerState{
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Next draws the next (emoji, word) pair, guaranteed to differ from
// the immediately previous one in both channels (when the pool has
// ≥2 entries, which is always true for the shipped seed).
func (t *creativeTickerState) Next() (emoji, word string) {
	if t == nil || len(creativeEmojiPool) == 0 || len(creativeWordPool) == 0 {
		return "", ""
	}
	emoji = t.pickDifferent(creativeEmojiPool, t.lastEmoji)
	word = t.pickDifferent(creativeWordPool, t.lastWord)
	t.lastEmoji = emoji
	t.lastWord = word
	return emoji, word
}

// pickDifferent draws a random element of `pool` that's not equal to
// `prev`. Falls back to any element when the pool has only one entry
// (degenerate case — callers don't hit it in practice).
func (t *creativeTickerState) pickDifferent(pool []string, prev string) string {
	if len(pool) == 1 {
		return pool[0]
	}
	for {
		pick := pool[t.rng.Intn(len(pool))]
		if pick != prev {
			return pick
		}
	}
}
