// Package ui holds terminal plumbing for the viewer: raw mode / alt screen
// lifecycle, key decoding, and status-line layout. None of it talks to
// herdr's socket API — that split keeps this package testable without a
// pty or a running herdr server.
package ui

import (
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

// Terminal owns the raw-mode / alternate-screen lifecycle for the process's
// own stdin/stdout. Every restoration step in Restore runs independently of
// the others (ARCH-review P1: viewer.py's single unguarded `finally` block
// could skip cursor/autowrap/alt-screen restoration entirely if tcsetattr
// raised first), and Restore is idempotent so it is safe to call from both
// a normal deferred cleanup and, e.g., a last-resort signal handler.
type Terminal struct {
	fd       int
	oldState *term.State
	entered  bool
	mu       sync.Mutex
}

// New creates a Terminal bound to the process's stdin file descriptor.
func New() *Terminal {
	return &Terminal{fd: int(os.Stdin.Fd())}
}

// Enter switches into the alternate screen, hides the cursor, disables
// autowrap, and puts stdin into raw mode (equivalent to Python's
// tty.setraw). Disabling autowrap is belt-and-suspenders against a status
// line that's one column too wide (from a width-calculation edge case)
// triggering a scroll that would push the status line up under the image
// layer — the same reasoning viewer.py used for DISABLE_AUTOWRAP.
func (t *Terminal) Enter() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	os.Stdout.WriteString(enterAltScreen + hideCursor + disableAutowrap)
	state, err := term.MakeRaw(t.fd)
	if err != nil {
		return err
	}
	t.oldState = state
	t.entered = true
	return nil
}

// Restore undoes Enter. Each step is attempted independently so a failure
// restoring termios attributes (e.g. stdin already closed) doesn't skip
// re-enabling autowrap, showing the cursor, or leaving the alt screen.
func (t *Terminal) Restore() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.entered {
		return
	}
	t.entered = false
	func() {
		defer func() { recover() }()
		if t.oldState != nil {
			_ = term.Restore(t.fd, t.oldState)
		}
	}()
	func() {
		defer func() { recover() }()
		os.Stdout.WriteString(enableAutowrap)
	}()
	func() {
		defer func() { recover() }()
		os.Stdout.WriteString(showCursor)
	}()
	func() {
		defer func() { recover() }()
		os.Stdout.WriteString(leaveAltScreen)
	}()
}

// ClearScreen clears the alternate screen's text content and homes the
// cursor. It does not touch any graphics layer — those are a separate
// compositing plane in herdr (ARCH-review "process／PTY／socket 三層關係").
func ClearScreen() {
	os.Stdout.WriteString(clearScreenSeq)
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
