package browser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/tim80411/herdr-pdf/internal/ui"
)

// Action is what the viewer should do after one key press.
type Action int

const (
	ActionNone Action = iota // key ignored; nothing to repaint
	ActionRedraw
	ActionOpen // Chosen() names the PDF to read
	ActionQuit
)

// helpTable is the single source of truth for browser mode's keys: the
// status line's hints and the "?" overlay are both generated from it, so a
// binding cannot be advertised in one place and forgotten in the other.
var helpTable = ui.HelpTable{
	Title: "瀏覽模式鍵位",
	Bindings: []ui.Binding{
		{Keys: "任何可列印字元", What: "即時過濾（不分大小寫子字串，可打中文）"},
		{Keys: "Backspace", What: "刪一個字；過濾字串已空時回上一層"},
		{Keys: "↑ ↓ / Ctrl-p Ctrl-n / Ctrl-k Ctrl-j", What: "上下移動"},
		{Keys: "Enter", What: "進入目錄，或開啟 PDF", Hint: "Enter 開啟"},
		{Keys: "Esc", What: "有過濾字就清除，沒有就離開", Hint: "Esc 清除/離開"},
		{Keys: "Tab", What: "編輯路徑：Tab 補全、Enter 前往或開檔、Esc 取消", Hint: "Tab 路徑"},
		{Keys: "Ctrl-h", What: "切換顯示隱藏項（. 開頭，預設不列）"},
		{Keys: "?", What: "顯示這張表", Hint: "? 說明"},
		{Keys: "Ctrl-c", What: "離開"},
	},
}

// Model is browser mode's whole state: the directory being listed, the
// filter typed over it, and the path bar when it is being edited.
type Model struct {
	dir        string
	entries    []Entry
	matches    []Entry
	cursor     int
	showHidden bool

	// filter holds raw input bytes rather than runes: ui.ReadKey reports one
	// byte at a time with no UTF-8 decoding, so a CJK filename character
	// arrives as two or three events and only reassembles if the bytes are
	// appended untouched. The path bar buffer works the same way.
	filter []byte

	editing bool
	edit    []byte
	// helping is the "?" overlay. It sits above both the listing and the
	// path bar, and any key dismisses it.
	helping bool
	// candidates is the last completion's matches, shown on the status row
	// when they did not collapse to a single path.
	candidates []string

	// scanErr replaces the listing when the directory itself is unreadable;
	// message is a one-shot complaint about the last action.
	scanErr string
	message string
	chosen  string
}

// New opens a browser on dir. A dir that cannot be read still produces a
// usable model — the error takes the listing's place rather than failing.
func New(dir string) *Model {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	m := &Model{dir: abs}
	m.reload()
	return m
}

// Dir returns the directory currently listed.
func (m *Model) Dir() string { return m.dir }

// Chosen returns the PDF path from the last ActionOpen.
func (m *Model) Chosen() string { return m.chosen }

// Matches returns the rows currently visible.
func (m *Model) Matches() []Entry { return m.matches }

// Cursor returns the cursor's index into Matches.
func (m *Model) Cursor() int { return m.cursor }

// Filter returns the filter string as typed.
func (m *Model) Filter() string { return string(m.filter) }

// Helping reports whether the key-table overlay is up.
func (m *Model) Helping() bool { return m.helping }

// Editing reports whether the path bar has focus.
func (m *Model) Editing() bool { return m.editing }

// EditBuffer returns the path bar's text.
func (m *Model) EditBuffer() string { return string(m.edit) }

// Selected reports the row under the cursor.
func (m *Model) Selected() (Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.matches) {
		return Entry{}, false
	}
	return m.matches[m.cursor], true
}

// SetMessage puts one line on the status row, for the viewer to report a
// file it could not open without tearing the listing down.
func (m *Model) SetMessage(msg string) { m.message = msg }

// Message returns the current status-row message.
func (m *Model) Message() string { return m.message }

// Focus parks the cursor on the row with this base name, if it is visible.
// Coming back from reading mode uses it to land on the file just read.
func (m *Model) Focus(name string) {
	for i, e := range m.matches {
		if e.Name == name || e.Name == name+"/" {
			m.cursor = i
			return
		}
	}
}

// reload rescans the current directory in place, keeping dir even when the
// scan fails so the path bar still shows where the user is.
func (m *Model) reload() {
	entries, err := Scan(m.dir, m.showHidden)
	if err != nil {
		m.entries, m.scanErr = nil, friendlyErr(err)
	} else {
		m.entries, m.scanErr = entries, ""
	}
	m.refilter()
}

// enter moves to dir, or complains and stays put: a directory the user
// cannot read should cost them their place no more than a typo does.
func (m *Model) enter(dir string) Action {
	entries, err := Scan(dir, m.showHidden)
	if err != nil {
		m.message = "無法讀取目錄: " + friendlyErr(err)
		return ActionRedraw
	}
	m.dir, m.entries = dir, entries
	m.scanErr, m.message = "", ""
	m.filter = nil
	m.cursor = 0
	m.refilter()
	return ActionRedraw
}

// friendlyErr strips the *PathError wrapper so the status line shows
// "permission denied" rather than the whole syscall sentence.
func friendlyErr(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

func (m *Model) refilter() {
	needle := strings.ToLower(string(m.filter))
	m.matches = make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		if needle == "" || strings.Contains(strings.ToLower(e.Name), needle) {
			m.matches = append(m.matches, e)
		}
	}
	if m.cursor >= len(m.matches) {
		m.cursor = 0
	}
}

// HandleKey advances the model by one key press.
func (m *Model) HandleKey(ev ui.KeyEvent) Action {
	if m.helping {
		// Any key returns to what was underneath; a closed pty still means
		// leave, since there is nobody left to read the table.
		if ev.Key == ui.KeyEOF {
			return ActionQuit
		}
		m.helping = false
		return ActionRedraw
	}
	if m.editing {
		return m.handleEditKey(ev)
	}
	switch ev.Key {
	case ui.KeyDown:
		return m.move(1)
	case ui.KeyUp:
		return m.move(-1)
	case ui.KeyHome:
		return m.moveTo(0)
	case ui.KeyEnd:
		return m.moveTo(len(m.matches) - 1)
	case ui.KeyLeft:
		return m.up()
	case ui.KeyRight:
		return m.activate()
	case ui.KeyEOF:
		return ActionQuit
	case ui.KeyRune:
		return m.handleListRune(ev.Rune)
	}
	return ActionNone
}

// handleListRune applies one key press to the listing. Typing wins: every
// printable character extends the filter, so no letter is reserved for
// navigation and no filename is unreachable. Movement and the rest live on
// control keys and the arrows instead.
func (m *Model) handleListRune(r rune) Action {
	switch r {
	case '\t':
		// The path bar starts empty so an absolute path can simply be typed.
		// A relative one resolves against the directory being listed, and
		// Tab completes within it, so pre-filling would only be in the way.
		m.editing = true
		m.edit = nil
		m.candidates = nil
		m.message = ""
		return ActionRedraw
	case '\r':
		return m.activate()
	case 0x0e, 0x0a: // Ctrl-n, Ctrl-j
		return m.move(1)
	case 0x10, 0x0b: // Ctrl-p, Ctrl-k
		return m.move(-1)
	case 0x08: // Ctrl-h toggles hidden entries. Backspace is 0x7f below.
		m.showHidden = !m.showHidden
		m.reload()
		return ActionRedraw
	case 0x7f: // Backspace
		return m.backspace()
	case 0x1b: // Escape: clear the filter, or leave when there is none
		if len(m.filter) == 0 {
			return ActionQuit
		}
		m.filter = nil
		m.message = ""
		m.refilter()
		return ActionRedraw
	case 0x03: // Ctrl-c
		return ActionQuit
	case '?': // ahead of the filter branch, so "?" never narrows the listing
		m.helping = true
		m.message = ""
		return ActionRedraw
	}
	if r >= 0x20 && r <= 0xff {
		m.filter = append(m.filter, byte(r))
		m.message = ""
		m.refilter()
		return ActionRedraw
	}
	return ActionNone
}

// backspace deletes one filter character, or leaves the directory when the
// filter is already empty — the same key backs out of a filter and then out
// of the directory it was narrowing.
func (m *Model) backspace() Action {
	if len(m.filter) == 0 {
		return m.up()
	}
	// One whole rune, not one byte: half of a CJK character is not a filter.
	_, size := utf8.DecodeLastRune(m.filter)
	if size < 1 {
		size = 1
	}
	m.filter = m.filter[:len(m.filter)-size]
	m.refilter()
	return ActionRedraw
}

func (m *Model) handleEditKey(ev ui.KeyEvent) Action {
	if ev.Key != ui.KeyRune {
		if ev.Key == ui.KeyEOF {
			return ActionQuit
		}
		return ActionNone
	}
	switch ev.Rune {
	case '\t':
		completed, cands := Complete(string(m.edit), m.dir)
		m.candidates = cands
		if completed == string(m.edit) && len(cands) < 2 {
			return ActionNone
		}
		m.edit = []byte(completed)
		return ActionRedraw
	case '\r':
		return m.submitEdit()
	case 0x1b: // Escape returns to the listing
		m.editing = false
		m.edit = nil
		m.candidates = nil
		return ActionRedraw
	case 0x03: // Ctrl-c leaves outright, from either mode
		return ActionQuit
	case 0x7f, 0x08: // Backspace, Ctrl-H
		if len(m.edit) == 0 {
			return ActionNone
		}
		// One whole rune, not one byte: half of a CJK directory name is not
		// a path.
		_, size := utf8.DecodeLastRune(m.edit)
		if size < 1 {
			size = 1
		}
		m.edit = m.edit[:len(m.edit)-size]
		return ActionRedraw
	}
	if ev.Rune >= 0x20 && ev.Rune <= 0xff {
		m.edit = append(m.edit, byte(ev.Rune))
		m.candidates = nil
		return ActionRedraw
	}
	return ActionNone
}

// submitEdit acts on the typed path: a directory is entered, a PDF is
// opened, anything else is reported without leaving the path bar's
// directory.
func (m *Model) submitEdit() Action {
	raw := strings.TrimSpace(string(m.edit))
	m.editing = false
	m.edit = nil
	m.candidates = nil
	if raw == "" {
		return ActionRedraw
	}

	target := ExpandHome(raw)
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.dir, target)
	}
	// Clean drops the trailing separator Tab completion leaves on a
	// directory, which would otherwise become part of m.dir and double up
	// the next time the path bar is opened.
	target = filepath.Clean(target)
	fi, err := os.Stat(target)
	if err != nil {
		m.message = "路徑不存在: " + raw
		return ActionRedraw
	}
	if fi.IsDir() {
		return m.enter(target)
	}
	if !strings.EqualFold(filepath.Ext(target), ".pdf") {
		m.message = "不是 PDF 檔: " + filepath.Base(target)
		return ActionRedraw
	}
	m.chosen = target
	return ActionOpen
}

// activate opens what the cursor is on: a directory is entered, a PDF is
// handed back to the viewer.
func (m *Model) activate() Action {
	e, ok := m.Selected()
	if !ok {
		return ActionNone
	}
	if e.IsDir {
		return m.enter(e.Path)
	}
	m.chosen = e.Path
	return ActionOpen
}

// up moves to the parent directory and parks the cursor on the directory
// just left, so walking up and back down lands where it started.
func (m *Model) up() Action {
	parent := Parent(m.dir)
	if parent == m.dir {
		return ActionNone
	}
	from := filepath.Base(m.dir)
	act := m.enter(parent)
	m.Focus(from)
	return act
}

func (m *Model) move(delta int) Action { return m.moveTo(m.cursor + delta) }

func (m *Model) moveTo(next int) Action {
	if len(m.matches) == 0 {
		return ActionNone
	}
	if next < 0 {
		next = 0
	}
	if next >= len(m.matches) {
		next = len(m.matches) - 1
	}
	if next == m.cursor {
		return ActionNone
	}
	m.cursor = next
	return ActionRedraw
}

// scrollStart returns the first visible row for a viewport listRows tall.
func (m *Model) scrollStart(listRows int) int {
	if listRows < 1 || m.cursor < listRows {
		return 0
	}
	return m.cursor - listRows + 1
}

// Render paints the whole pane: a path bar on row 1, the listing between,
// and a status row at the bottom. Rows are cleared individually and
// addressed absolutely, so a repaint cannot scroll the pane even when a CJK
// name lands near the right edge.
func (m *Model) Render(cols, rows int) string {
	if m.helping {
		return helpTable.RenderHelp(cols, rows)
	}
	if cols < 1 {
		cols = 1
	}
	if rows < 3 {
		rows = 3
	}
	listRows := rows - 2
	start := m.scrollStart(listRows)

	var b strings.Builder
	b.WriteString(ui.RenderStatusLine(1, m.PathBar(cols)))

	for i := 0; i < listRows; i++ {
		row := i + 2
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", row)
		if i == 0 && m.scanErr != "" {
			b.WriteString("  " + ui.TruncateToWidth("無法讀取此目錄: "+m.scanErr, cols-3))
			continue
		}
		idx := start + i
		if idx >= len(m.matches) {
			continue
		}
		// Two columns for the cursor marker, one held back from the right
		// edge so a row cannot touch the last cell.
		label := ui.TruncateToWidth(m.matches[idx].Name, cols-3)
		if idx == m.cursor {
			b.WriteString(ui.Reverse("> " + label))
			continue
		}
		b.WriteString("  " + label)
	}

	b.WriteString(ui.RenderStatusLine(rows, m.StatusLine(cols)))
	return b.String()
}

// PathBar is row 1: the directory being listed, or the path being typed.
// It never claims the last column, since filling it on a CJK path would
// trip the terminal's autowrap and scroll the listing.
func (m *Model) PathBar(cols int) string {
	prefix, text := "", m.dir
	if m.editing {
		// The terminal cursor is hidden in raw mode, so the caret is drawn.
		prefix, text = "路徑> ", string(m.edit)+"_"
	}
	avail := cols - 1 - ui.DisplayWidth(prefix)
	if avail < 0 {
		avail = 0
	}
	// Tail-first: a path too long to fit is more useful from its deep end,
	// and in edit mode the caret has to stay visible.
	body := ui.TruncateKeepTail(text, avail)
	pad := avail - ui.DisplayWidth(body)
	if pad < 0 {
		pad = 0
	}
	return prefix + body + strings.Repeat(" ", pad)
}

// StatusLine is the bottom row: the position in the listing, whatever the
// last action had to say, and the keys worth knowing.
func (m *Model) StatusLine(cols int) string {
	if m.editing {
		return ui.StatusLine("編輯路徑", m.candidateHint(cols), "[Tab 補全  Enter 前往  Esc 取消]", cols)
	}
	shown := 0
	if len(m.matches) > 0 {
		shown = m.cursor + 1
	}
	left := fmt.Sprintf("%d/%d", shown, len(m.matches))

	mid := ""
	switch {
	case m.message != "":
		mid = m.message
	case len(m.filter) > 0:
		mid = "篩選: " + string(m.filter)
	}
	return ui.StatusLine(left, mid, helpTable.StatusHints(), cols)
}

// candidateHint lists the first few completions, so an ambiguous Tab says
// what it was ambiguous between rather than just stopping. It is capped to
// half the row: the key hints beside it stay more useful than a fourth name.
func (m *Model) candidateHint(cols int) string {
	if len(m.candidates) < 2 {
		return ""
	}
	const show = 3
	head := m.candidates
	if len(head) > show {
		head = head[:show]
	}
	hint := "候選: " + strings.Join(head, "  ")
	if len(m.candidates) > show {
		hint += fmt.Sprintf("  …共 %d", len(m.candidates))
	}
	return ui.TruncateToWidth(hint, cols/2)
}
