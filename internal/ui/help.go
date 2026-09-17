package ui

import (
	"fmt"
	"strings"
)

// Binding is one row of a mode's key table: the keys to press and what they
// do. Hint is the short form the status line carries; a binding with no
// Hint is documented only on the help screen.
type Binding struct {
	Keys string
	What string
	Hint string
}

// HelpTable is one mode's bindings plus the title its help screen shows.
// Both the status-line hints and the "?" screen are generated from it, so a
// key cannot be advertised in one place and forgotten in the other.
type HelpTable struct {
	Title    string
	Bindings []Binding
}

// StatusHints is the bracketed segment a status line carries, built from
// whichever bindings declare a hint. It returns "" for a table that
// declares none.
func (h HelpTable) StatusHints() string {
	var parts []string
	for _, b := range h.Bindings {
		if b.Hint != "" {
			parts = append(parts, b.Hint)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, "  ") + "]"
}

// RenderHelp draws the help screen over the whole pane: a reverse-video
// title, one two-column row per binding, and a footer saying how to leave.
// Rows are cleared individually and addressed absolutely, so a repaint
// cannot scroll the pane even when a CJK description lands near the edge.
// Bindings past the available rows are dropped rather than wrapped.
func (h HelpTable) RenderHelp(cols, rows int) string {
	if cols < 1 {
		cols = 1
	}
	if rows < 3 {
		rows = 3
	}

	keyWidth := 0
	for _, b := range h.Bindings {
		if w := DisplayWidth(b.Keys); w > keyWidth {
			keyWidth = w
		}
	}
	// The key column never takes more than half the row; past that the
	// descriptions stop being readable, which is the point of the screen.
	if half := (cols - 1) / 2; keyWidth > half {
		keyWidth = half
	}

	var b strings.Builder
	b.WriteString(RenderStatusLine(1, StatusLine(h.Title, "", "", cols)))

	// Two leading columns, two between the columns, and one column held
	// back from the right edge.
	whatBudget := cols - 1 - 2 - keyWidth - 2
	for i := 0; i < rows-2; i++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", i+2)
		if i >= len(h.Bindings) {
			continue
		}
		keys := TruncateToWidth(h.Bindings[i].Keys, keyWidth)
		pad := keyWidth - DisplayWidth(keys)
		if pad < 0 {
			pad = 0
		}
		b.WriteString("  " + keys + strings.Repeat(" ", pad) + "  " +
			TruncateToWidth(h.Bindings[i].What, whatBudget))
	}

	b.WriteString(RenderStatusLine(rows, StatusLine("按任意鍵返回", "", "", cols)))
	return b.String()
}
