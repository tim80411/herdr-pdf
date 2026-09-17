package ui

import "testing"

func TestDisplayWidthCJK(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want int
	}{
		{"ascii", "report.pdf", 10},
		{"cjk filename", "簡章(中文版).pdf", 16}, // rune count is 11, display width is 16
		{"empty", "", 0},
		{"mixed cjk ascii", "報告.pdf", 8}, // 報告=4 + ".pdf"=4
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DisplayWidth(c.s); got != c.want {
				t.Errorf("DisplayWidth(%q) = %d, want %d", c.s, got, c.want)
			}
		})
	}
}

func TestTruncateToWidth(t *testing.T) {
	cases := []struct {
		name     string
		s        string
		maxWidth int
		want     string
	}{
		{"fits exactly", "abc", 3, "abc"},
		{"no truncation needed", "abc", 10, "abc"},
		{"ascii truncation", "abcdefgh", 5, "abcd…"},
		{"zero width", "abc", 0, ""},
		{"negative width", "abc", -1, ""},
		{"width one always ellipsis", "abcdef", 1, "…"},
		{"cjk truncation respects column budget", "簡章(中文版).pdf", 5, "簡章…"}, // budget=4 after reserving ellipsis: 簡(2)+章(2)=4 fits, "(" (1) would push to 5
		{"never exceeds width for empty budget case", "x", 1, "x"},         // exact fit, no truncation needed
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateToWidth(c.s, c.maxWidth)
			if got != c.want {
				t.Errorf("TruncateToWidth(%q, %d) = %q, want %q", c.s, c.maxWidth, got, c.want)
			}
			if w := DisplayWidth(got); c.maxWidth >= 0 && w > c.maxWidth {
				t.Errorf("TruncateToWidth(%q, %d) = %q has display width %d > %d", c.s, c.maxWidth, got, w, c.maxWidth)
			}
		})
	}
}

func TestStatusLineWidth(t *testing.T) {
	// The status line must never claim the last column (autowrap guard),
	// regardless of how wide the filename is.
	cols := 40
	name := "一份非常非常非常長的中文檔名用來測試截斷邏輯.pdf"
	mid := "頁 1/18  [j/k 翻頁 g/G 首尾 q 離開]"
	right := "stream 750x1061 42ms"

	line := StatusLine(name, mid, right, cols)
	if w := DisplayWidth(line); w != cols-1 {
		t.Errorf("StatusLine width = %d, want exactly %d (cols-1)", w, cols-1)
	}
}

func TestStatusLineNarrowPane(t *testing.T) {
	// A pane narrower than mid+right alone must still not overflow.
	cols := 10
	name := "x.pdf"
	mid := "頁 1/18  [j/k 翻頁 g/G 首尾 q 離開]"
	right := "stream 750x1061 42ms"

	line := StatusLine(name, mid, right, cols)
	if w := DisplayWidth(line); w > cols-1 {
		t.Errorf("StatusLine width = %d, want <= %d", w, cols-1)
	}
}

func TestTruncateKeepTail(t *testing.T) {
	cases := []struct {
		name     string
		s        string
		maxWidth int
		want     string
	}{
		{"fits", "abc", 3, "abc"},
		{"keeps the tail", "/home/user/deep/dir", 10, "…/deep/dir"},
		{"zero width", "abc", 0, ""},
		{"width one always ellipsis", "abcdef", 1, "…"},
		{"cjk keeps the last two wide runes", "招生資料簡章", 5, "…簡章"}, // budget=4 after the ellipsis: 簡(2)+章(2)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateKeepTail(c.s, c.maxWidth)
			if got != c.want {
				t.Errorf("TruncateKeepTail(%q, %d) = %q, want %q", c.s, c.maxWidth, got, c.want)
			}
			if w := DisplayWidth(got); w > c.maxWidth && c.maxWidth >= 0 {
				t.Errorf("result %q has display width %d > %d", got, w, c.maxWidth)
			}
		})
	}
}
