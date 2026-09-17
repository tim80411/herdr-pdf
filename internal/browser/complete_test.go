package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// completed runs one round of completion and returns only the new text.
func completed(t *testing.T, input, base string) string {
	t.Helper()
	out, _ := Complete(input, base)
	return out
}

func TestCompleteSingleCandidateGainsSeparatorForADirectory(t *testing.T) {
	root := fixture(t, "Downloads/", "report.pdf")

	if got := completed(t, "Dow", root); got != "Downloads"+string(filepath.Separator) {
		t.Errorf("Complete(%q) = %q, want the directory with a trailing separator", "Dow", got)
	}
	if got := completed(t, "rep", root); got != "report.pdf" {
		t.Errorf("Complete(%q) = %q, want report.pdf", "rep", got)
	}
}

func TestCompleteStopsAtTheSharedPrefixAndReportsTheCandidates(t *testing.T) {
	root := fixture(t, "report-2024.pdf", "report-2025.pdf", "notes.pdf")

	got, cands := Complete("rep", root)
	if got != "report-202" {
		t.Errorf("Complete(%q) = %q, want the shared prefix report-202", "rep", got)
	}
	// Sorted, so the status row lists them in a stable order.
	if !equal(cands, []string{"report-2024.pdf", "report-2025.pdf"}) {
		t.Errorf("candidates = %v, want both reports", cands)
	}
}

func TestCompleteKeepsTheDirectoryHalfAndWalksDown(t *testing.T) {
	root := fixture(t, "scans/", "scans/invoice.pdf")

	// An absolute input keeps everything left of the last separator.
	input := filepath.Join(root, "scans") + string(filepath.Separator) + "inv"
	want := filepath.Join(root, "scans") + string(filepath.Separator) + "invoice.pdf"
	if got := completed(t, input, root); got != want {
		t.Errorf("Complete(%q) = %q, want %q", input, got, want)
	}
}

func TestCompleteIgnoresNonPDFFilesButNotDirectories(t *testing.T) {
	root := fixture(t, "data.txt", "data-dir/", "datasheet.pdf")

	// "data" matches all three; only the directory and the PDF are
	// candidates, so the shared prefix stops before they diverge.
	if got := completed(t, "data", root); got != "data" {
		t.Errorf("Complete(%q) = %q, want it unchanged (candidates diverge)", "data", got)
	}
	if got := completed(t, "datas", root); got != "datasheet.pdf" {
		t.Errorf("Complete(%q) = %q, want datasheet.pdf", "datas", got)
	}
	if got := completed(t, "data-", root); got != "data-dir"+string(filepath.Separator) {
		t.Errorf("Complete(%q) = %q, want the directory", "data-", got)
	}
}

func TestCompleteLeavesUnmatchableInputAlone(t *testing.T) {
	root := fixture(t, "report.pdf")

	got, cands := Complete("zzz", root)
	if got != "zzz" || cands != nil {
		t.Errorf("Complete(%q) = %q/%v, want it unchanged with no candidates", "zzz", got, cands)
	}
	missing := filepath.Join(root, "nope") + string(filepath.Separator) + "x"
	if got := completed(t, missing, root); got != missing {
		t.Errorf("Complete on a missing directory rewrote %q to %q", missing, got)
	}
}

func TestCompleteRevealsHiddenEntriesOnlyAfterTheDot(t *testing.T) {
	root := fixture(t, ".hidden/", "shown.pdf")

	if got := completed(t, "", root); got != "shown.pdf" {
		t.Errorf("Complete(%q) = %q, want only the visible entry", "", got)
	}
	if got := completed(t, ".hid", root); got != ".hidden"+string(filepath.Separator) {
		t.Errorf("Complete(%q) = %q, want the hidden directory", ".hid", got)
	}
}

func TestCompleteMatchesCaseInsensitively(t *testing.T) {
	root := fixture(t, "Report.pdf")

	if got := completed(t, "rep", root); got != "Report.pdf" {
		t.Errorf("Complete(%q) = %q, want Report.pdf", "rep", got)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := ExpandHome("~"); got != home {
		t.Errorf("ExpandHome(\"~\") = %q, want %q", got, home)
	}
	if got := ExpandHome("~/Downloads"); got != filepath.Join(home, "Downloads") {
		t.Errorf("ExpandHome(\"~/Downloads\") = %q", got)
	}
	for _, unchanged := range []string{"/abs/path", "relative", "~user/dir"} {
		if got := ExpandHome(unchanged); got != unchanged {
			t.Errorf("ExpandHome(%q) = %q, want it unchanged", unchanged, got)
		}
	}
}
