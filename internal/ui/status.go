package ui

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
)

// DisplayWidth returns the terminal column width of s: 2 per East Asian
// Wide/Fullwidth rune (CJK, full-width punctuation, ...), 0 for
// zero-width/combining runes, 1 otherwise. Undercounting a CJK filename
// would overflow the status line and trigger an autowrap scroll that
// pushes it up under the image layer.
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

// TruncateKeepTail truncates s to at most maxWidth display columns by
// dropping runes from the front, marking the cut with a leading ellipsis.
// It is the right direction for a path or a line being typed: the deepest
// directory and the caret are the parts worth keeping.
func TruncateKeepTail(s string, maxWidth int) string {
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
	runes := []rune(s)
	width := 0
	cut := len(runes)
	for i := len(runes) - 1; i >= 0; i-- {
		w := runewidth.RuneWidth(runes[i])
		if width+w > budget {
			break
		}
		width += w
		cut = i
	}
	return "…" + string(runes[cut:])
}

// StatusLine lays out the bottom row's content (without paint escapes — see
// RenderStatusLine): name (left, truncated to fit), mid (center — page
// indicator + key hints), right (render diagnostics), padded to
// exactly cols-1 display columns. It never fills the last column, since
// doing so on a CJK-heavy name would trip the terminal's autowrap and scroll the pane.
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

// Reverse wraps s in reverse video, for the selected row of a list. It
// keeps every SGR escape in this package rather than spreading raw
// sequences across callers.
func Reverse(s string) string {
	return reverseVideo + s + resetSGR
}
