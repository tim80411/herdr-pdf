// Package ui holds terminal plumbing for the viewer (raw mode, alt screen,
// key decoding, status-line layout) with no dependency on herdr.
package ui

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	enterAltScreen  = "\x1b[?1049h"
	leaveAltScreen  = "\x1b[?1049l"
	hideCursor      = "\x1b[?25l"
	showCursor      = "\x1b[?25h"
	clearScreenSeq  = "\x1b[2J\x1b[H"
	disableAutowrap = "\x1b[?7l"
	enableAutowrap  = "\x1b[?7h"
	reverseVideo    = "\x1b[7m"
	resetSGR        = "\x1b[0m"
)

// lockedWriter makes each Write atomic with respect to the others.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Stdout is the one writer for the pane's output. Text and Kitty graphics
// escapes share that stream now, so a whole escape must go out in a single
// Write here, never interleaved with a status line from another goroutine.
var Stdout io.Writer = &lockedWriter{w: os.Stdout}

// Screen is the terminal surface the viewer draws on. Taking it as an
// interface lets the shutdown path be driven without a tty.
type Screen interface {
	Enter() error
	Restore()
	WriteString(s string)
	ClearScreen()
}

// Terminal owns the raw-mode / alternate-screen lifecycle for the process's
// own stdin/stdout. Restore is idempotent and each of its steps runs
// independently, so it's safe to call from both a deferred cleanup and a
// last-resort signal handler.
type Terminal struct {
	fd       int
	out      io.Writer
	raw      bool
	oldState *term.State
	entered  bool
	mu       sync.Mutex
}

// New creates a Terminal bound to the process's stdin file descriptor.
func New() *Terminal {
	return &Terminal{fd: int(os.Stdin.Fd()), out: Stdout, raw: true}
}

// NewForOutput creates a Terminal that writes its control sequences to w and
// leaves termios untouched.
func NewForOutput(w io.Writer) *Terminal {
	return &Terminal{out: w}
}

// Enter puts stdin into raw mode, then switches into the alternate screen,
// hides the cursor, and disables autowrap. Raw mode goes first because it is
// the step that can fail; a failure afterwards rolls the whole thing back so
// the caller never has to clean up a half-entered terminal. Disabling
// autowrap guards against a one-column status-line overflow scrolling the
// pane and pushing the status line up under the image layer.
func (t *Terminal) Enter() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.raw {
		state, err := term.MakeRaw(t.fd)
		if err != nil {
			return err
		}
		t.oldState = state
	}
	if _, err := io.WriteString(t.out, enterAltScreen+hideCursor+disableAutowrap); err != nil {
		t.undo()
		return err
	}
	t.entered = true
	return nil
}

// Restore undoes Enter.
func (t *Terminal) Restore() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.entered {
		return
	}
	t.entered = false
	t.undo()
}

// undo attempts each teardown step independently so one failure (e.g. stdin
// already closed) doesn't skip the rest. Callers hold mu.
func (t *Terminal) undo() {
	if t.oldState != nil {
		state := t.oldState
		t.oldState = nil
		func() {
			defer func() { recover() }()
			_ = term.Restore(t.fd, state)
		}()
	}
	for _, seq := range []string{enableAutowrap, showCursor, leaveAltScreen} {
		func() {
			defer func() { recover() }()
			_, _ = io.WriteString(t.out, seq)
		}()
	}
}

// WriteString sends raw output to the screen.
func (t *Terminal) WriteString(s string) {
	_, _ = io.WriteString(t.out, s)
}

// ClearScreen clears the alternate screen's text content and homes the
// cursor. Callers free their Kitty images explicitly instead of relying on
// the clear to do it.
func (t *Terminal) ClearScreen() {
	t.WriteString(clearScreenSeq)
}

// Size reports this process's controlling terminal size in cells via
// TIOCGWINSZ, i.e. the pane's current cols/rows.
func Size() (cols, rows int, err error) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// SizePixels reports the pane's size in pixels as the terminal states it, 0
// when it does not say. Callers treat it as a hint, not as layout truth.
func SizePixels() (w, h int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Xpixel), int(ws.Ypixel)
}
