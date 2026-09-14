package openpath

import (
	"path/filepath"
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
	// Matches resolve_path.py: invalid JSON is swallowed, not raised.
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
