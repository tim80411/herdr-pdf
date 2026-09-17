package ui

import (
	"strings"
	"testing"
)

var testTable = HelpTable{
	Title: "測試鍵位",
	Bindings: []Binding{
		{Keys: "Enter", What: "開啟", Hint: "Enter 開啟"},
		{Keys: "Ctrl-h", What: "切換顯示隱藏項"},
		{Keys: "?", What: "顯示這張表", Hint: "? 說明"},
	},
}

func TestStatusHintsOnlyIncludesBindingsThatDeclareOne(t *testing.T) {
	got := testTable.StatusHints()
	if got != "[Enter 開啟  ? 說明]" {
		t.Errorf("StatusHints() = %q", got)
	}
	if strings.Contains(got, "Ctrl-h") {
		t.Error("a binding with no Hint leaked into the status line")
	}
	if got := (HelpTable{Title: "x"}).StatusHints(); got != "" {
		t.Errorf("StatusHints() on an empty table = %q, want empty", got)
	}
}

func TestRenderHelpLayout(t *testing.T) {
	out := testTable.RenderHelp(40, 6)

	// Title on row 1, four body rows, footer on row 6, each addressed
	// absolutely and cleared.
	for _, row := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H", "\x1b[4;1H", "\x1b[5;1H", "\x1b[6;1H"} {
		if !strings.Contains(out, row) {
			t.Errorf("render is missing an absolute move to %q", row)
		}
	}
	for _, want := range []string{"測試鍵位", "Enter", "開啟", "Ctrl-h", "切換顯示隱藏項", "按任意鍵返回"} {
		if !strings.Contains(out, want) {
			t.Errorf("render is missing %q", want)
		}
	}
}

func TestRenderHelpNeverClaimsTheLastColumn(t *testing.T) {
	wide := HelpTable{
		Title: "很長很長很長很長很長很長很長很長的標題",
		Bindings: []Binding{
			{Keys: "一個非常長的按鍵名稱用來測試截斷", What: "一段非常長的中文說明用來測試截斷行為"},
		},
	}
	for _, cols := range []int{10, 20, 40, 80} {
		out := wide.RenderHelp(cols, 4)
		for _, line := range strings.Split(out, "\x1b[2K") {
			body := strings.TrimSuffix(strings.TrimPrefix(line, reverseVideo), resetSGR)
			if i := strings.Index(body, "\x1b["); i >= 0 {
				body = body[:i]
			}
			if w := DisplayWidth(body); w > cols-1 {
				t.Errorf("cols=%d: line %q has display width %d, want <= %d", cols, body, w, cols-1)
			}
		}
	}
}

func TestRenderHelpDropsRowsThatDoNotFit(t *testing.T) {
	// Three bindings, but only one body row available.
	out := testTable.RenderHelp(40, 3)
	if !strings.Contains(out, "Enter") {
		t.Error("the first binding should still be shown")
	}
	if strings.Contains(out, "切換顯示隱藏項") {
		t.Error("a binding past the available rows was drawn anyway")
	}
}
