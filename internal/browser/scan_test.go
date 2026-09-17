package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fixture builds a directory tree: names ending in "/" become directories,
// everything else a file.
func fixture(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		full := filepath.Join(root, n)
		if len(n) > 0 && n[len(n)-1] == '/' {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", n, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", n, err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return root
}

func names(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScanListsDirectoriesFirstThenPDFsEachByName(t *testing.T) {
	root := fixture(t, "zebra/", "Alpha/", "b.pdf", "A.PDF", "notes.txt", "image.png")

	got, err := Scan(root, false)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []string{"Alpha/", "zebra/", "A.PDF", "b.pdf"}
	if g := names(got); !equal(g, want) {
		t.Errorf("Scan = %v, want %v", g, want)
	}
}

func TestScanHiddenEntriesNeedTheToggle(t *testing.T) {
	root := fixture(t, ".git/", ".secret.pdf", "visible.pdf")

	got, err := Scan(root, false)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if g := names(got); !equal(g, []string{"visible.pdf"}) {
		t.Errorf("Scan(showHidden=false) = %v, want only visible.pdf", g)
	}

	got, err = Scan(root, true)
	if err != nil {
		t.Fatalf("Scan(showHidden): %v", err)
	}
	if g := names(got); !equal(g, []string{".git/", ".secret.pdf", "visible.pdf"}) {
		t.Errorf("Scan(showHidden=true) = %v", g)
	}
}

func TestScanFollowsSymlinkedDirectoryAndSkipsDanglingOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	root := fixture(t, "real/", "real/inside.pdf")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "broken")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := Scan(root, false)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// A link to a directory lists as a directory; a dangling one is dropped.
	if g := names(got); !equal(g, []string{"linked/", "real/"}) {
		t.Errorf("Scan = %v, want linked/ and real/", g)
	}
}

func TestScanUnreadableDirectoryIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if _, err := Scan(locked, false); err == nil {
		t.Fatal("Scan on an unreadable directory returned no error")
	}
}

func TestParentStopsAtTheRoot(t *testing.T) {
	root := string(filepath.Separator)
	if got := Parent(root); got != root {
		t.Errorf("Parent(%q) = %q, want it to stay put", root, got)
	}
	if got := Parent(filepath.Join(root, "a", "b")); got != filepath.Join(root, "a") {
		t.Errorf("Parent = %q, want /a", got)
	}
}
