// Package browser is the viewer's file-browsing mode: a path bar and a
// directory listing that picks the PDF to read. It exists because the
// "open" action usually fires from a keybinding with nothing selected — a
// selection made in the outer terminal emulator never reaches herdr — so
// the pane has to be able to ask which file, rather than fail.
//
// Nothing here draws graphics; browsing is plain text, and the graphics
// stream is only opened once a file has been chosen.
package browser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one row of the listing: a subdirectory or a PDF.
type Entry struct {
	// Name is what the row shows: the base name, with "/" appended for a
	// directory so the two kinds are distinguishable without color.
	Name  string
	Path  string // absolute
	IsDir bool
}

// Scan lists dir's subdirectories followed by its PDFs, each group sorted
// by name (case-insensitively, with a case-sensitive tie-break so the order
// is stable). Symlinks are resolved, so a link to a directory is listed and
// entered as one. Dot-entries are included only when showHidden is set.
func Scan(dir string, showHidden bool) ([]Entry, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var dirs, files []Entry
	for _, e := range ents {
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			// A symlink reports its own type, not its target's; resolving it
			// is what lets a linked directory be entered.
			fi, statErr := os.Stat(full)
			if statErr != nil {
				continue // dangling link
			}
			isDir = fi.IsDir()
		}
		switch {
		case isDir:
			dirs = append(dirs, Entry{Name: name + "/", Path: full, IsDir: true})
		case strings.EqualFold(filepath.Ext(name), ".pdf"):
			files = append(files, Entry{Name: name, Path: full})
		}
	}

	sortByName(dirs)
	sortByName(files)
	return append(dirs, files...), nil
}

func sortByName(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		li, lj := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if li == lj {
			return entries[i].Name < entries[j].Name
		}
		return li < lj
	})
}

// Parent returns dir's parent, or dir itself at the filesystem root so
// holding the up key cannot walk off the top.
func Parent(dir string) string {
	parent := filepath.Dir(dir)
	if parent == dir {
		return dir
	}
	return parent
}
