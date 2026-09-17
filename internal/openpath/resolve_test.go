package openpath

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveArgvWins(t *testing.T) {
	got, err := Resolve("/abs/path/file.pdf", "https://ignored", `{"clicked_url":"ignored"}`, "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/abs/path/file.pdf" {
		t.Errorf("got %q, want argv path to win", got)
	}
}

func TestResolveClickedURLEnvFallback(t *testing.T) {
	got, err := Resolve("", "/abs/from/env.pdf", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/abs/from/env.pdf" {
		t.Errorf("got %q, want %q", got, "/abs/from/env.pdf")
	}
}

func TestResolveContextClickedURLFallback(t *testing.T) {
	got, err := Resolve("", "", `{"clicked_url":"/abs/from/context.pdf"}`, "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/abs/from/context.pdf" {
		t.Errorf("got %q, want %q", got, "/abs/from/context.pdf")
	}
}

func TestResolveSelectedTextIsLastResort(t *testing.T) {
	got, err := Resolve("", "", `{"selected_text":"/abs/selected.pdf"}`, "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/abs/selected.pdf" {
		t.Errorf("got %q, want %q", got, "/abs/selected.pdf")
	}
}

func TestResolveNoCandidateIsError(t *testing.T) {
	if _, err := Resolve("", "", "", "/cwd"); err == nil {
		t.Fatal("expected error when no candidate is available")
	}
}

func TestResolveMalformedContextJSONIsNotFatal(t *testing.T) {
	// Invalid JSON is swallowed, not raised.
	got, err := Resolve("/abs/file.pdf", "", "{not valid json", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/abs/file.pdf" {
		t.Errorf("got %q, want %q", got, "/abs/file.pdf")
	}
}

func TestResolveFileURLStripped(t *testing.T) {
	got, err := Resolve("file:///Users/tim/a%20b.pdf", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/Users/tim/a b.pdf" {
		t.Errorf("got %q, want %q", got, "/Users/tim/a b.pdf")
	}
}

func TestResolveRelativePathUsesFocusedPaneCwd(t *testing.T) {
	got, err := Resolve("sub/file.pdf", "", `{"focused_pane_cwd":"/home/user/project"}`, "/ignored")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/home/user/project", "sub/file.pdf")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveRelativePathFallsBackToCwd(t *testing.T) {
	got, err := Resolve("sub/file.pdf", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/cwd", "sub/file.pdf")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveEmptyCandidateAfterStripIsError(t *testing.T) {
	if _, err := Resolve("file://", "", "", "/cwd"); err == nil {
		t.Fatal("expected error for a file:// URL with no path")
	}
}

func TestResolveRejectsRemoteFileURLHost(t *testing.T) {
	// file://host/path names a file on another machine; opening the local
	// path of the same name would be the wrong document.
	for _, in := range []string{
		"file://example.com/Users/tim/a.pdf",
		"file://192.168.1.10/share/a.pdf",
	} {
		if got, err := Resolve(in, "", "", "/cwd"); err == nil {
			t.Errorf("Resolve(%q) = %q, want an error for a remote host", in, got)
		}
	}
}

func TestResolveAcceptsLocalhostFileURL(t *testing.T) {
	got, err := Resolve("file://localhost/Users/tim/a.pdf", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/Users/tim/a.pdf" {
		t.Errorf("got %q, want %q", got, "/Users/tim/a.pdf")
	}
}

func TestResolveLeavesBarePathEscapesAlone(t *testing.T) {
	// Only a file:// URL is percent-decoded. A plain path keeps its literal
	// characters, so a filename that really contains "%41" still opens.
	got, err := Resolve("/docs/%41%20report.pdf", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/docs/%41%20report.pdf" {
		t.Errorf("got %q, want the bare path decoded literally", got)
	}
}

func TestResolveRejectsNULInPath(t *testing.T) {
	for _, in := range []string{
		"/docs/evil\x00.pdf",
		"file:///docs/evil%00.pdf",
	} {
		if got, err := Resolve(in, "", "", "/cwd"); err == nil {
			t.Errorf("Resolve(%q) = %q, want a rejection", in, got)
		} else if !strings.Contains(err.Error(), "非法路徑") {
			t.Errorf("Resolve(%q) error = %v, want it to say the path is invalid", in, err)
		}
	}
}

func TestResolveTrimsOnlyTrailingNewline(t *testing.T) {
	// A clipboard or shell pipeline adds a newline; spaces in a filename are
	// the user's and must survive.
	got, err := Resolve("/docs/ spaced name .pdf\n", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/docs/ spaced name .pdf" {
		t.Errorf("got %q, want the surrounding spaces kept", got)
	}

	got, err = Resolve("/docs/trailing.pdf\r\n", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/docs/trailing.pdf" {
		t.Errorf("got %q, want the CRLF stripped", got)
	}
}

func TestResolveDoesNotRequireAPDFExtension(t *testing.T) {
	// Extension policy belongs to the "open" subcommand, not to resolution.
	got, err := Resolve("/docs/notes.txt", "", "", "/cwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/docs/notes.txt" {
		t.Errorf("got %q, want the path resolved regardless of extension", got)
	}
}
