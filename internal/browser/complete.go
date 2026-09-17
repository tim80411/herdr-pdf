package browser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Complete does one round of shell-style path completion on the text of the
// path bar: it extends input as far as the candidates agree, and no
// further. base is the directory a relative input is resolved against.
//
// The first return is what the path bar should now hold — input itself when
// nothing matches, so a typo is never silently rewritten. A completed
// directory gains a trailing separator, which is what makes repeated Tab
// walk down a tree. The second is every candidate that matched, so the
// caller can show them when they did not collapse to one.
func Complete(input, base string) (string, []string) {
	dirPart, namePart := splitPath(input)
	lookup := ExpandHome(dirPart)
	if lookup == "" {
		lookup = base
	} else if !filepath.IsAbs(lookup) {
		lookup = filepath.Join(base, lookup)
	}

	ents, err := os.ReadDir(lookup)
	if err != nil {
		return input, nil
	}

	// Hidden entries stay reachable, but only once the user has typed the
	// dot that asks for them.
	wantHidden := strings.HasPrefix(namePart, ".")
	var names []string
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") && !wantHidden {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(namePart)) {
			continue
		}
		if isDirEntry(lookup, e) {
			names = append(names, name+string(filepath.Separator))
			continue
		}
		if strings.EqualFold(filepath.Ext(name), ".pdf") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return input, nil
	}
	sort.Strings(names)
	return dirPart + commonPrefix(names), names
}

// isDirEntry reports whether e is a directory, following a symlink.
func isDirEntry(dir string, e os.DirEntry) bool {
	if e.Type()&os.ModeSymlink == 0 {
		return e.IsDir()
	}
	fi, err := os.Stat(filepath.Join(dir, e.Name()))
	return err == nil && fi.IsDir()
}

// splitPath cuts input at the last separator, keeping the separator with
// the directory half so the two concatenate back to the original.
func splitPath(input string) (dirPart, namePart string) {
	idx := strings.LastIndex(input, string(filepath.Separator))
	if idx < 0 {
		return "", input
	}
	return input[:idx+1], input[idx+1:]
}

// commonPrefix returns the longest prefix shared by all names, compared
// case-insensitively but returned in the first candidate's own case: on a
// case-insensitive filesystem "Rep" and "report" do share a prefix, and
// truncating to the agreed length keeps the result openable.
func commonPrefix(names []string) string {
	prefix := names[0]
	for _, name := range names[1:] {
		limit := len(prefix)
		if len(name) < limit {
			limit = len(name)
		}
		i := 0
		for i < limit && lowerASCII(prefix[i]) == lowerASCII(name[i]) {
			i++
		}
		prefix = prefix[:i]
	}
	return prefix
}

func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// ExpandHome resolves a leading "~". The path bar is typed by a person, so
// it is the one place in this plugin where a tilde is expected to work.
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
}
