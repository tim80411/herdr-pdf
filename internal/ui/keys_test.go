package ui

import (
	"bufio"
	"bytes"
	"testing"
)

func readAll(t *testing.T, input []byte, want []KeyEvent) {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(input))
	for i, w := range want {
		got, err := ReadKey(r)
		if err != nil {
			t.Fatalf("event %d: unexpected error: %v", i, err)
		}
		if got != w {
			t.Fatalf("event %d: got %+v, want %+v", i, got, w)
		}
	}
}

func TestReadKeyPlainRune(t *testing.T) {
	readAll(t, []byte("jkq"), []KeyEvent{
		{Key: KeyRune, Rune: 'j'},
		{Key: KeyRune, Rune: 'k'},
		{Key: KeyRune, Rune: 'q'},
	})
}

func TestReadKeyArrows(t *testing.T) {
	// \x1b[A / B / C / D — the classic 3-byte arrow sequences.
	readAll(t, []byte("\x1b[A\x1b[B\x1b[C\x1b[D"), []KeyEvent{
		{Key: KeyUp},
		{Key: KeyDown},
		{Key: KeyRight},
		{Key: KeyLeft},
	})
}

func TestReadKeyPageUpDown(t *testing.T) {
	// \x1b[5~ (PageUp) and \x1b[6~ (PageDown) are 4 bytes — the case
	// viewer.py's fixed-3-byte read_key() mishandled (ARCH-review P2),
	// leaving the trailing '~' to be misread as the next keypress.
	readAll(t, []byte("\x1b[5~\x1b[6~"), []KeyEvent{
		{Key: KeyPageUp},
		{Key: KeyPageDown},
	})
}

func TestReadKeyPageUpThenRuneNotCorrupted(t *testing.T) {
	// Regression for the exact bug: after a 4-byte CSI sequence, the next
	// read must be a clean keypress, not a leftover '~'.
	readAll(t, []byte("\x1b[5~j"), []KeyEvent{
		{Key: KeyPageUp},
		{Key: KeyRune, Rune: 'j'},
	})
}

func TestReadKeyHomeEnd(t *testing.T) {
	readAll(t, []byte("\x1b[H\x1b[F\x1b[1~\x1b[4~"), []KeyEvent{
		{Key: KeyHome},
		{Key: KeyEnd},
		{Key: KeyHome},
		{Key: KeyEnd},
	})
}

func TestReadKeyUnknownCSIStillConsumed(t *testing.T) {
	// An unrecognized final byte must still be fully consumed so it
	// doesn't corrupt the next read.
	readAll(t, []byte("\x1b[99Zk"), []KeyEvent{
		{Key: KeyUnknown},
		{Key: KeyRune, Rune: 'k'},
	})
}

func TestReadKeyBareEscape(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{0x1b}))
	got, err := ReadKey(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != (KeyEvent{Key: KeyRune, Rune: 0x1b}) {
		t.Fatalf("got %+v, want bare escape rune", got)
	}
}

func TestReadKeyEOF(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader(nil))
	got, err := ReadKey(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Key != KeyEOF {
		t.Fatalf("got %+v, want KeyEOF", got)
	}
}

func TestReadKeyEOFMidEscapeSequence(t *testing.T) {
	// EOF arriving mid-sequence (pty closed while a partial escape was
	// buffered) must still report KeyEOF, not an error or a bogus key.
	r := bufio.NewReader(bytes.NewReader([]byte("\x1b[5")))
	got, err := ReadKey(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Key != KeyEOF {
		t.Fatalf("got %+v, want KeyEOF", got)
	}
}

func TestStartKeyReaderClosesOnEOF(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("j")))
	ch := StartKeyReader(r)

	first := <-ch
	if first != (KeyEvent{Key: KeyRune, Rune: 'j'}) {
		t.Fatalf("got %+v, want 'j'", first)
	}
	second := <-ch
	if second.Key != KeyEOF {
		t.Fatalf("got %+v, want KeyEOF", second)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after KeyEOF")
	}
}
