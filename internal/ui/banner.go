package ui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var cyanStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF"))
var dimCyanStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#008B8B"))
var taglineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF")).Italic(true)

// bannerLineType marks how each line should be rendered.
type bannerLineType int

const (
	lineArt      bannerLineType = iota // buddha ascii art
	lineBlank                          // empty spacer
	lineTitle                          // center-aligned large title
	lineSubtitle                       // center-aligned subtitle
)

type bannerLine struct {
	text     string
	lineType bannerLineType
}

var buddhaLines = []bannerLine{
	{`                    _ooOoo_`, lineArt},
	{`                   o8888888o`, lineArt},
	{`                   88" . "88`, lineArt},
	{`                   (| -_- |)`, lineArt},
	{`                   O\  =  /O`, lineArt},
	{`                ____/'---'\____`, lineArt},
	{`              .'  \\|     |//  '.`, lineArt},
	{`             /  \\|||  :  |||//  \`, lineArt},
	{`            /  _||||| -:- |||||_  \`, lineArt},
	{`            |   | \\\  -  /'| |   |`, lineArt},
	{`            | \_|  ''\---/''  |_/ |`, lineArt},
	{`            \  .-\__ '-' ___/-. /`, lineArt},
	{`          ___'. .'  /--.--\  '. .'___`, lineArt},
	{`       ."" '<  '.___\_<|>_/___.' >'  "".`, lineArt},
	{`      | | :  '- \'.;'\ _ /';.'/ -'  : | |`, lineArt},
	{`      \  \ '_.   \_ __\ /__ _/   ._' /  /`, lineArt},
	{`  ======'-.____'.___ \_____/___.-'____.-'======`, lineArt},
	{`                     '=---='`, lineArt},
	{``, lineBlank},
	{` ____    ____  _  _  _  _   ____  ____`, lineTitle},
	{`| __ )  / () \| \| || \| | / () \|_  /`, lineTitle},
	{`|___ \ /__/\__\_\___|_\___|/__/\__\/__/ `, lineTitle},
	{`   |___|                            CLI`, lineTitle},
	{``, lineBlank},
	{`Your AI Code Agent  |  Type to start  |  /help for commands`, lineSubtitle},
}

const taglineText = `"For all code gurus in the universe!!"`

var taglineRunes = []rune(taglineText)

// Animation intervals
const (
	bannerAnimInterval  = 50 * time.Millisecond // 25 lines × 50ms ≈ 1.25s (1.5× speed)
	taglineAnimInterval = 26 * time.Millisecond // typewriter speed per character (1.5× speed)
)

// bannerTickMsg drives the line-by-line drop animation.
type bannerTickMsg struct{}

// taglineTickMsg drives the left-to-right typewriter animation.
type taglineTickMsg struct{}

func bannerTick() tea.Cmd {
	return tea.Tick(bannerAnimInterval, func(time.Time) tea.Msg {
		return bannerTickMsg{}
	})
}

func taglineTick() tea.Cmd {
	return tea.Tick(taglineAnimInterval, func(time.Time) tea.Msg {
		return taglineTickMsg{}
	})
}

func totalBannerLines() int {
	return len(buddhaLines)
}

func totalTaglineChars() int {
	return len(taglineRunes)
}

// maxLineWidth returns the rune-width of the widest line.
func maxLineWidth() int {
	max := 0
	for _, l := range buddhaLines {
		w := lipgloss.Width(l.text)
		if w > max {
			max = w
		}
	}
	// Also consider the tagline
	if tw := lipgloss.Width(taglineText); tw > max {
		max = tw
	}
	return max
}

// centerPad centers text within targetWidth.
func centerPad(s string, targetWidth int) string {
	w := lipgloss.Width(s)
	if w >= targetWidth {
		return s
	}
	left := (targetWidth - w) / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", targetWidth-w-left)
}

// rightPad pads text with trailing spaces to fill targetWidth.
func rightPad(s string, targetWidth int) string {
	w := lipgloss.Width(s)
	if w >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-w)
}

// renderBanner renders the banner with animation state.
//   - visibleLines: how many drop-down lines are shown (0..25)
//   - visibleChars: how many tagline characters are typed (0..len), -1 = not started yet
func renderBanner(width, height, visibleLines, visibleChars int) string {
	if visibleLines <= 0 {
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, "")
	}
	if visibleLines > len(buddhaLines) {
		visibleLines = len(buddhaLines)
	}

	blockWidth := maxLineWidth()

	var lines []string
	for i := 0; i < visibleLines; i++ {
		bl := buddhaLines[i]
		switch bl.lineType {
		case lineTitle:
			centered := centerPad(bl.text, blockWidth)
			lines = append(lines, lipgloss.NewStyle().
				Foreground(lipgloss.Color("#00FFFF")).
				Bold(true).
				Render(centered))
		case lineSubtitle:
			centered := centerPad(bl.text, blockWidth)
			lines = append(lines, dimCyanStyle.Render(centered))
		case lineBlank:
			lines = append(lines, strings.Repeat(" ", blockWidth))
		case lineArt:
			padded := rightPad(bl.text, blockWidth)
			lines = append(lines, cyanStyle.Render(padded))
		}
	}

	// Tagline typewriter (only after all lines are drawn)
	if visibleChars >= 0 && visibleLines >= len(buddhaLines) {
		lines = append(lines, strings.Repeat(" ", blockWidth)) // blank spacer

		chars := visibleChars
		if chars > len(taglineRunes) {
			chars = len(taglineRunes)
		}
		partial := string(taglineRunes[:chars])
		// Add blinking cursor while typing
		if chars < len(taglineRunes) {
			partial += "_"
		}
		centered := centerPad(partial, blockWidth)
		lines = append(lines, taglineStyle.Render(centered))
	}

	banner := strings.Join(lines, "\n")

	centered := lipgloss.Place(
		width,
		height,
		lipgloss.Center,
		lipgloss.Center,
		banner,
	)

	return strings.TrimRight(centered, "\n")
}
