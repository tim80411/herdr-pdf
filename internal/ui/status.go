package ui

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
)

// DisplayWidth returns the terminal column width of s: 2 per East Asian
// Wide/Fullwidth rune (CJK, full-width punctuation, ...), 0 for
// zero-width/combining runes, 1 otherwise. This mirrors viewer.py's
// hand-rolled display_width(), which existed because Python's len()
// undercounts CJK filenames by half — enough to overflow the status line
// past the pane width and trigger an autowrap-induced scroll that pushed
// the status line up under the image layer (viewer.py:148-162).
// go-runewidth implements the same East Asian Width Unicode annex the
// Python version was hand-coding, and is what herdr's own Rust terminal
// emulator (libghostty-vt) is measuring against on the other end.
func DisplayWidth(s string) int {
	return runewidth.StringWidth(s)
}

// TruncateToWidth truncates s to at most maxWidth display columns,
// appending an ellipsis if anything was cut. It never returns something
// wider than maxWidth, even for maxWidth 0 or 1.
func TruncateToWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if DisplayWidth(s) <= maxWidth {
		return s
	}
	if maxWidth == 1 {
		return "…"
	}
	budget := maxWidth - 1 // reserve 1 column for "…"
	var b strings.Builder
	width := 0
	for _, r := range s {
		cw := runewidth.RuneWidth(r)
		if width+cw > budget {
			break
		}
		b.WriteRune(r)
		width += cw
	}
	return b.String() + "…"
}

// StatusLine lays out the bottom row's content (without the escape
// sequences to actually paint it — see RenderStatusLine): name (left,
// truncated to fit), mid (center — page indicator + key hints), right
// (render/stream diagnostics), padded to exactly cols-1 display columns.
//
// It never fills the last column: on a CJK-heavy filename, overflowing the
// last column would trip the terminal's autowrap and scroll the pane,
// which is the same status-line-overflow bug DISABLE_AUTOWRAP in
// Terminal.Enter is a second line of defense against.
func StatusLine(name, mid, right string, cols int) string {
	avail := cols - 1
	if avail < 0 {
		avail = 0
	}
	const gap = "  "
	reserved := DisplayWidth(gap)*2 + DisplayWidth(mid) + DisplayWidth(right)
	nameBudget := avail - reserved
	if nameBudget < 0 {
		nameBudget = 0
	}
	left := TruncateToWidth(name, nameBudget)

	content := left + gap + mid + gap + right
	if DisplayWidth(content) > avail {
		// Pane narrower than mid+right alone (a very small split) — fall
		// back to trimming the whole line rather than overflowing.
		content = TruncateToWidth(content, avail)
	}
	pad := avail - DisplayWidth(content)
	if pad < 0 {
		pad = 0
	}
	return content + strings.Repeat(" ", pad)
}

// RenderStatusLine returns the escape sequence that paints line at the
// bottom row (1-based), in reverse video: move cursor, clear the line,
// reverse video on, text, SGR reset.
func RenderStatusLine(row int, line string) string {
	return fmt.Sprintf("\x1b[%d;1H\x1b[2K%s%s%s", row, reverseVideo, line, resetSGR)
}
