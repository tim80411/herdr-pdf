package browser

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tim80411/herdr-pdf/internal/ui"
)

// Control bytes the listing binds, spelled out so the tests read as the
// keys a person presses.
const (
	keyTab       = "\t"
	keyEnter     = "\r"
	keyEsc       = "\x1b"
	keyBackspace = "\x7f"
	keyCtrlC     = "\x03"
	keyCtrlH     = "\x08"
	keyCtrlN     = "\x0e"
	keyCtrlP     = "\x10"
	keyCtrlJ     = "\x0a"
	keyCtrlK     = "\x0b"
	keyDownArrow = "\x1b[B"
	keyUpArrow   = "\x1b[A"
)

// drive feeds raw terminal bytes through the real key decoder into the
// model, which is how the pane actually receives input. It returns the last
// action, stopping early on one that leaves the listing.
func drive(t *testing.T, m *Model, input string) Action {
	t.Helper()
	r := bufio.NewReader(strings.NewReader(input))
	last := ActionNone
	for {
		ev, err := ui.ReadKey(r)
		if err != nil {
			t.Fatalf("ReadKey: %v", err)
		}
		if ev.Key == ui.KeyEOF {
			return last
		}
		last = m.HandleKey(ev)
		if last == ActionOpen || last == ActionQuit {
			return last
		}
	}
}

func TestMoveWithArrowsAndControlKeys(t *testing.T) {
	m := New(fixture(t, "a.pdf", "b.pdf", "c.pdf"))

	for _, down := range []string{keyDownArrow, keyCtrlN, keyCtrlJ} {
		m2 := New(fixture(t, "a.pdf", "b.pdf", "c.pdf"))
		drive(t, m2, down)
		if m2.Cursor() != 1 {
			t.Errorf("%q moved to %d, want 1", down, m2.Cursor())
		}
	}
	for _, up := range []string{keyUpArrow, keyCtrlP, keyCtrlK} {
		m2 := New(fixture(t, "a.pdf", "b.pdf", "c.pdf"))
		drive(t, m2, keyCtrlN+keyCtrlN+up)
		if m2.Cursor() != 1 {
			t.Errorf("%q moved to %d, want 1", up, m2.Cursor())
		}
	}

	drive(t, m, keyCtrlN+keyCtrlN)
	if m.Cursor() != 2 {
		t.Fatalf("cursor = %d, want the last row", m.Cursor())
	}
	if act := drive(t, m, keyCtrlN); act != ActionNone {
		t.Errorf("moving past the end returned %v, want ActionNone", act)
	}
}

func TestEveryPrintableCharacterFiltersIncludingTheOldCommandLetters(t *testing.T) {
	// The whole point of the fzf-style keymap: no letter is reserved, so a
	// file named only of former command letters is still reachable.
	m := New(fixture(t, "jkq.pdf", "other.pdf"))

	drive(t, m, "jkq")
	if m.Filter() != "jkq" {
		t.Fatalf("Filter = %q, want jkq", m.Filter())
	}
	if n := names(m.Matches()); !equal(n, []string{"jkq.pdf"}) {
		t.Fatalf("matches = %v, want only jkq.pdf", n)
	}
	// Movement must not have happened while typing.
	if m.Cursor() != 0 {
		t.Errorf("cursor = %d, want 0 (typing must not move)", m.Cursor())
	}
}

func TestFilterIsCaseInsensitiveSubstring(t *testing.T) {
	m := New(fixture(t, "Report-Final.pdf", "notes.pdf", "報告.pdf"))

	drive(t, m, "RE")
	if n := names(m.Matches()); !equal(n, []string{"Report-Final.pdf"}) {
		t.Fatalf("matches = %v, want only Report-Final.pdf", n)
	}
}

func TestFilterAcceptsCJKTypedAsRawBytes(t *testing.T) {
	m := New(fixture(t, "報告.pdf", "notes.pdf"))

	// "報" reaches the process as three separate bytes; the filter must
	// reassemble them rather than re-encode each as its own rune.
	drive(t, m, "報")
	if m.Filter() != "報" {
		t.Fatalf("Filter = %q, want 報", m.Filter())
	}
	if n := names(m.Matches()); !equal(n, []string{"報告.pdf"}) {
		t.Fatalf("matches = %v, want only 報告.pdf", n)
	}
}

func TestEscapeIsTwoStageClearThenLeave(t *testing.T) {
	m := New(fixture(t, "a.pdf", "b.pdf"))

	drive(t, m, "a")
	if len(m.Matches()) != 1 {
		t.Fatalf("matches = %d, want the filter applied", len(m.Matches()))
	}
	// First Escape clears the filter and stays.
	if act := drive(t, m, keyEsc); act != ActionRedraw {
		t.Fatalf("Escape with a filter returned %v, want ActionRedraw", act)
	}
	if m.Filter() != "" || len(m.Matches()) != 2 {
		t.Fatalf("filter %q with %d matches, want it cleared", m.Filter(), len(m.Matches()))
	}
	// Second Escape, with nothing left to clear, leaves.
	if act := drive(t, m, keyEsc); act != ActionQuit {
		t.Errorf("Escape with no filter returned %v, want ActionQuit", act)
	}
}

func TestBackspaceDeletesAWholeRuneThenLeavesTheDirectory(t *testing.T) {
	root := fixture(t, "scans/", "scans/報告.pdf")
	m := New(root)

	drive(t, m, keyEnter) // into scans/
	if filepath.Base(m.Dir()) != "scans" {
		t.Fatalf("Dir = %q, want scans", m.Dir())
	}

	drive(t, m, "報")
	if m.Filter() != "報" {
		t.Fatalf("Filter = %q, want 報", m.Filter())
	}
	// With a filter, Backspace deletes one whole character.
	drive(t, m, keyBackspace)
	if m.Filter() != "" {
		t.Fatalf("Filter = %q, want it emptied by one Backspace", m.Filter())
	}
	// With the filter empty, the same key backs out of the directory.
	drive(t, m, keyBackspace)
	if m.Dir() != root {
		t.Fatalf("Dir = %q, want to have gone up to %q", m.Dir(), root)
	}
	if sel, ok := m.Selected(); !ok || sel.Name != "scans/" {
		t.Errorf("cursor after going up = %+v, want it on scans/", sel)
	}
}

func TestBackspaceAtTheFilesystemRootDoesNothing(t *testing.T) {
	m := New(string(filepath.Separator))
	if act := drive(t, m, keyBackspace); act != ActionNone {
		t.Errorf("Backspace at the root returned %v, want ActionNone", act)
	}
}

func TestEnterDirectoryAndOpenPDF(t *testing.T) {
	root := fixture(t, "scans/", "scans/inside.pdf", "top.pdf")
	m := New(root)

	// Row 0 is the directory: directories sort before files.
	if act := drive(t, m, keyEnter); act != ActionRedraw {
		t.Fatalf("Enter on a directory returned %v, want ActionRedraw", act)
	}
	if n := names(m.Matches()); !equal(n, []string{"inside.pdf"}) {
		t.Fatalf("listing after entering = %v", n)
	}
	if act := drive(t, m, keyEnter); act != ActionOpen {
		t.Fatalf("Enter on a PDF returned %v, want ActionOpen", act)
	}
	if want := filepath.Join(root, "scans", "inside.pdf"); m.Chosen() != want {
		t.Errorf("Chosen = %q, want %q", m.Chosen(), want)
	}
}

func TestCtrlHTogglesHiddenEntries(t *testing.T) {
	m := New(fixture(t, ".git/", "shown.pdf"))

	if n := names(m.Matches()); !equal(n, []string{"shown.pdf"}) {
		t.Fatalf("initial listing = %v, want hidden entries omitted", n)
	}
	drive(t, m, keyCtrlH)
	if n := names(m.Matches()); !equal(n, []string{".git/", "shown.pdf"}) {
		t.Fatalf("after Ctrl-h = %v, want the hidden directory listed", n)
	}
	drive(t, m, keyCtrlH)
	if n := names(m.Matches()); !equal(n, []string{"shown.pdf"}) {
		t.Errorf("after a second Ctrl-h = %v, want it hidden again", n)
	}
}

func TestCtrlCAndEOFLeave(t *testing.T) {
	m := New(fixture(t, "a.pdf"))
	if act := drive(t, m, keyCtrlC); act != ActionQuit {
		t.Errorf("Ctrl-c returned %v, want ActionQuit", act)
	}
	// Ctrl-c leaves from the path bar too, where Escape only cancels.
	m2 := New(fixture(t, "a.pdf"))
	drive(t, m2, keyTab)
	if act := drive(t, m2, keyCtrlC); act != ActionQuit {
		t.Errorf("Ctrl-c in the path bar returned %v, want ActionQuit", act)
	}
	m3 := New(fixture(t, "a.pdf"))
	if act := m3.HandleKey(ui.KeyEvent{Key: ui.KeyEOF}); act != ActionQuit {
		t.Errorf("KeyEOF returned %v, want ActionQuit", act)
	}
}

func TestPathBarCompletesAndNavigates(t *testing.T) {
	root := fixture(t, "scans/", "scans/invoice.pdf", "top.pdf")
	m := New(root)

	drive(t, m, keyTab)
	if !m.Editing() {
		t.Fatal("Tab did not open the path bar")
	}
	if m.EditBuffer() != "" {
		t.Fatalf("path bar starts at %q, want it empty so a path can be typed", m.EditBuffer())
	}

	// Tab completes against the directory being listed, Enter walks into it.
	drive(t, m, "sca"+keyTab)
	if want := "scans" + string(filepath.Separator); m.EditBuffer() != want {
		t.Fatalf("after Tab buffer = %q, want %q", m.EditBuffer(), want)
	}
	drive(t, m, keyEnter)
	if m.Editing() {
		t.Error("path bar stayed open after Enter")
	}
	if filepath.Base(m.Dir()) != "scans" {
		t.Fatalf("Dir = %q, want scans", m.Dir())
	}

	// A completed PDF opens straight from the path bar.
	drive(t, m, keyTab)
	drive(t, m, "inv"+keyTab)
	if act := drive(t, m, keyEnter); act != ActionOpen {
		t.Fatalf("Enter on a completed PDF returned %v, want ActionOpen", act)
	}
	if want := filepath.Join(root, "scans", "invoice.pdf"); m.Chosen() != want {
		t.Errorf("Chosen = %q, want %q", m.Chosen(), want)
	}
}

func TestPathBarAcceptsAbsoluteAndHomeRelativePaths(t *testing.T) {
	root := fixture(t, "scans/", "top.pdf")
	m := New(root)

	drive(t, m, keyTab)
	drive(t, m, filepath.Join(root, "scans")+keyEnter)
	if filepath.Base(m.Dir()) != "scans" {
		t.Fatalf("an absolute path landed at %q, want scans", m.Dir())
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	drive(t, m, keyTab)
	drive(t, m, "~"+keyEnter)
	if m.Dir() != home {
		t.Errorf("\"~\" landed at %q, want %q", m.Dir(), home)
	}
}

func TestPathBarShowsCandidatesWhenCompletionIsAmbiguous(t *testing.T) {
	m := New(fixture(t, "report-2024.pdf", "report-2025.pdf", "notes.pdf"))

	drive(t, m, keyTab)
	drive(t, m, "rep"+keyTab)
	if m.EditBuffer() != "report-202" {
		t.Fatalf("buffer = %q, want the shared prefix", m.EditBuffer())
	}
	hint := m.StatusLine(120)
	for _, want := range []string{"候選:", "report-2024.pdf", "report-2025.pdf"} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(hint, want) {
				t.Errorf("status line %q is missing %q", hint, want)
			}
		})
	}
	// Typing again invalidates the list rather than leaving it stale.
	drive(t, m, "4")
	if strings.Contains(m.StatusLine(120), "候選:") {
		t.Error("candidates survived a keystroke")
	}
}

func TestPathBarRejectsWhatItCannotOpen(t *testing.T) {
	root := fixture(t, "notes.txt")
	m := New(root)

	drive(t, m, keyTab)
	drive(t, m, "missing.pdf"+keyEnter)
	if !strings.Contains(m.Message(), "路徑不存在") {
		t.Errorf("message = %q, want it to report a missing path", m.Message())
	}

	drive(t, m, keyTab)
	drive(t, m, "notes.txt"+keyEnter)
	if !strings.Contains(m.Message(), "不是 PDF") {
		t.Errorf("message = %q, want it to reject a non-PDF", m.Message())
	}
	if m.Dir() != root {
		t.Errorf("Dir moved to %q on a rejected path", m.Dir())
	}
}

func TestPathBarBackspaceRemovesOneWholeRuneAndEscapeCancels(t *testing.T) {
	m := New(fixture(t, "a.pdf"))

	drive(t, m, keyTab)
	drive(t, m, "報")
	if m.EditBuffer() != "報" {
		t.Fatalf("buffer = %q, want one CJK character", m.EditBuffer())
	}
	drive(t, m, keyBackspace)
	if m.EditBuffer() != "" {
		t.Fatalf("buffer after backspace = %q, want empty", m.EditBuffer())
	}
	drive(t, m, keyEsc)
	if m.Editing() {
		t.Error("Escape did not close the path bar")
	}
}

func TestUnreadableDirectoryShowsAnErrorInsteadOfCrashing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	root := fixture(t, "locked/")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	m := New(root)
	// Entering it must complain and stay put rather than empty the listing.
	if act := drive(t, m, keyEnter); act != ActionRedraw {
		t.Fatalf("Enter on an unreadable directory returned %v", act)
	}
	if m.Dir() != root {
		t.Errorf("Dir = %q, want to have stayed at %q", m.Dir(), root)
	}
	if !strings.Contains(m.Message(), "無法讀取目錄") {
		t.Errorf("message = %q, want it to report the failure", m.Message())
	}

	// Opening straight onto it keeps a usable model, with the error in
	// place of the listing.
	direct := New(locked)
	if len(direct.Matches()) != 0 {
		t.Errorf("listing = %v, want empty", names(direct.Matches()))
	}
	if out := direct.Render(60, 6); !strings.Contains(out, "無法讀取此目錄") {
		t.Error("render does not show the unreadable-directory line")
	}
}

func TestRenderLayoutAndStatusLine(t *testing.T) {
	m := New(fixture(t, "scans/", "一份很長的中文檔名用來測試截斷.pdf", "b.pdf"))
	out := m.Render(30, 5)

	// Path bar on row 1, three listing rows, status row at the bottom.
	for _, row := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H", "\x1b[4;1H", "\x1b[5;1H"} {
		if !strings.Contains(out, row) {
			t.Errorf("render is missing an absolute move to %q", row)
		}
	}
	if !strings.Contains(out, "> ") {
		t.Error("render has no cursor marker")
	}
	if !strings.Contains(out, "…") {
		t.Error("the over-long CJK name was not truncated")
	}

	status := m.StatusLine(60)
	for _, want := range []string{"1/3", "[Enter 開啟", "Tab 路徑", "Esc 清除/離開", "? 說明]"} {
		if !strings.Contains(status, want) {
			t.Errorf("status line %q is missing %q", status, want)
		}
	}
	if w := ui.DisplayWidth(status); w != 59 {
		t.Errorf("status line width = %d, want 59 (cols-1)", w)
	}

	drive(t, m, "b")
	if !strings.Contains(m.StatusLine(60), "篩選: b") {
		t.Errorf("status line %q does not show the filter", m.StatusLine(60))
	}

	drive(t, m, keyEsc)
	drive(t, m, keyTab)
	if bar := m.PathBar(60); !strings.Contains(bar, "路徑>") || !strings.Contains(bar, "_") {
		t.Errorf("edit-mode path bar = %q, want a prompt and a caret", bar)
	}
	if st := m.StatusLine(60); !strings.Contains(st, "Tab 補全") {
		t.Errorf("edit-mode status line = %q, want the completion hint", st)
	}
}

func TestPathBarKeepsTheTailOfALongPath(t *testing.T) {
	m := New(fixture(t, "a.pdf"))
	bar := m.PathBar(20)

	// A path too long to fit is more useful from its deep end.
	if !strings.HasPrefix(bar, "…") {
		t.Errorf("path bar = %q, want it truncated from the front", bar)
	}
	if w := ui.DisplayWidth(bar); w != 19 {
		t.Errorf("path bar width = %d, want 19 (cols-1)", w)
	}
}

func TestQuestionMarkShowsTheKeyTableAndAnyKeyReturns(t *testing.T) {
	m := New(fixture(t, "a.pdf", "b.pdf"))

	if act := drive(t, m, "?"); act != ActionRedraw {
		t.Fatalf("? returned %v, want ActionRedraw", act)
	}
	if !m.Helping() {
		t.Fatal("? did not open the overlay")
	}
	// "?" must not have reached the filter on its way in.
	if m.Filter() != "" {
		t.Errorf("Filter = %q, want ? to be a command rather than a filter character", m.Filter())
	}
	out := m.Render(60, 12)
	if !strings.Contains(out, "瀏覽模式鍵位") || !strings.Contains(out, "按任意鍵返回") {
		t.Error("the overlay does not render the key table")
	}
	if strings.Contains(out, "a.pdf") {
		t.Error("the listing is still drawn underneath the overlay")
	}

	// Any key returns, and the listing comes back intact.
	if act := drive(t, m, "x"); act != ActionRedraw {
		t.Fatalf("a key during the overlay returned %v, want ActionRedraw", act)
	}
	if m.Helping() {
		t.Fatal("the overlay stayed up")
	}
	if m.Filter() != "" {
		t.Errorf("Filter = %q, want the dismissing key to be swallowed", m.Filter())
	}
	if out := m.Render(60, 12); !strings.Contains(out, "a.pdf") {
		t.Error("the listing did not come back after the overlay closed")
	}
}

func TestOverlayIsDismissedByTheKeysThatOtherwiseLeave(t *testing.T) {
	// Escape and q close the table rather than quitting, since the overlay
	// is what the key is aimed at.
	for _, key := range []string{keyEsc, "q", "?"} {
		m := New(fixture(t, "a.pdf"))
		drive(t, m, "?")
		if act := m.HandleKey(ui.KeyEvent{Key: ui.KeyRune, Rune: rune(key[0])}); act != ActionRedraw {
			t.Errorf("%q during the overlay returned %v, want ActionRedraw", key, act)
		}
		if m.Helping() {
			t.Errorf("%q did not close the overlay", key)
		}
	}
	// A closed pty still ends the session.
	m := New(fixture(t, "a.pdf"))
	drive(t, m, "?")
	if act := m.HandleKey(ui.KeyEvent{Key: ui.KeyEOF}); act != ActionQuit {
		t.Errorf("EOF during the overlay returned %v, want ActionQuit", act)
	}
}

func TestOverlayCoversThePathBarToo(t *testing.T) {
	m := New(fixture(t, "a.pdf"))
	drive(t, m, keyTab)
	if !m.Editing() {
		t.Fatal("Tab did not open the path bar")
	}
	// "?" is a legal path character, so while editing it must type, not
	// open the overlay.
	drive(t, m, "?")
	if m.Helping() {
		t.Fatal("? opened the overlay from inside the path bar")
	}
	if m.EditBuffer() != "?" {
		t.Errorf("EditBuffer = %q, want ? typed into the path", m.EditBuffer())
	}
}

func TestScrollStartKeepsTheCursorVisible(t *testing.T) {
	var files []string
	for i := 0; i < 10; i++ {
		files = append(files, string(rune('a'+i))+".pdf")
	}
	m := New(fixture(t, files...))

	drive(t, m, strings.Repeat(keyCtrlN, 6))
	if m.Cursor() != 6 {
		t.Fatalf("cursor = %d, want 6", m.Cursor())
	}
	if got := m.scrollStart(4); got != 3 {
		t.Errorf("scrollStart(4) = %d, want 3", got)
	}
	if got := m.scrollStart(19); got != 0 {
		t.Errorf("scrollStart(19) = %d, want 0 (everything fits)", got)
	}
}
