package ui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// errWriter records everything written but reports every write as failed,
// standing in for a terminal that goes away mid-sequence. The buffer is a
// field rather than embedded so io.WriteString cannot route around Write.
type errWriter struct{ buf bytes.Buffer }

func (w *errWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return 0, errors.New("write failed")
}

func (w *errWriter) String() string { return w.buf.String() }

func TestEnterLeavesTerminalUntouchedWhenRawModeFails(t *testing.T) {
	// A pipe is not a tty, so MakeRaw fails. Nothing visible may have been
	// written by then, or the caller is handed an error while the screen is
	// already switched and the cursor already hidden.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	var buf bytes.Buffer
	term := &Terminal{fd: int(r.Fd()), out: &buf, raw: true}
	if enterErr := term.Enter(); enterErr == nil {
		t.Fatal("Enter on a pipe should fail")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q before raw mode succeeded", buf.String())
	}
	term.Restore()
	if buf.Len() != 0 {
		t.Errorf("Restore after a failed Enter wrote %q", buf.String())
	}
}

func TestEnterRollsBackWhenWritingSequencesFails(t *testing.T) {
	w := &errWriter{}
	term := &Terminal{out: w}
	if err := term.Enter(); err == nil {
		t.Fatal("Enter should report the write failure")
	}
	got := w.String()
	for _, seq := range []string{enableAutowrap, showCursor, leaveAltScreen} {
		if !strings.Contains(got, seq) {
			t.Errorf("rollback did not emit %q, got %q", seq, got)
		}
	}
}

func TestRestoreEmitsEveryResetSequence(t *testing.T) {
	var buf bytes.Buffer
	term := NewForOutput(&buf)
	if err := term.Enter(); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, enterAltScreen) {
		t.Fatalf("Enter did not switch screens, got %q", got)
	}
	buf.Reset()

	term.Restore()
	got := buf.String()
	for _, seq := range []string{leaveAltScreen, showCursor, enableAutowrap} {
		if !strings.Contains(got, seq) {
			t.Errorf("Restore did not emit %q, got %q", seq, got)
		}
	}

	buf.Reset()
	term.Restore()
	if buf.Len() != 0 {
		t.Errorf("a second Restore wrote %q, want nothing", buf.String())
	}
}

// Terminal must satisfy the interface the viewer depends on.
var _ Screen = (*Terminal)(nil)
