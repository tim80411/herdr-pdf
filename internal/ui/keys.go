package ui

import (
	"bufio"
	"io"
)

// Key classifies a decoded key event. Only the keys this viewer binds to
// anything get their own constant; anything else decodes as KeyUnknown so
// the CSI parser can still fully consume unrecognized sequences.
type Key int

const (
	KeyRune Key = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyUnknown
	// KeyEOF is reported when the underlying reader hits EOF (e.g. the pty
	// master closed). The caller's main loop must treat this as "stop", not
	// "no key available right now" — viewer.py's read_key() conflated the
	// two by returning "" for both timeout and EOF, which spun the main
	// loop at up to ~570k iterations/0.5s against a closed fd
	// (ARCH-review P0, reproduced in scratchpad/eoftest.py).
	KeyEOF
)

// KeyEvent is one decoded key press. Rune is meaningful only when Key is
// KeyRune.
type KeyEvent struct {
	Key  Key
	Rune rune
}

// ReadKey reads one logical key press from r: either a single byte (ASCII
// keys only matter for this viewer's bindings, so raw bytes are returned
// as-is rather than fully UTF-8 decoded) or a CSI escape sequence
// (ESC '[' ... final-byte).
//
// It always reads through to a CSI final byte (0x40-0x7E) before
// returning, which is the fix for viewer.py's read_key(): that function
// read a fixed 3 bytes for any escape sequence, so a 4-byte sequence like
// PageUp's \x1b[5~ left the trailing '~' in the input buffer to be
// misparsed as the *next* keypress (ARCH-review P2, viewer.py:409-424).
func ReadKey(r *bufio.Reader) (KeyEvent, error) {
	b, err := r.ReadByte()
	if err != nil {
		if err == io.EOF {
			return KeyEvent{Key: KeyEOF}, nil
		}
		return KeyEvent{}, err
	}
	if b != 0x1b {
		return KeyEvent{Key: KeyRune, Rune: rune(b)}, nil
	}

	// A bare Escape with nothing buffered right behind it: this viewer has
	// no binding for standalone Escape, so report it as a rune and let the
	// caller ignore it rather than block waiting to disambiguate from a
	// slow-typed CSI sequence.
	if r.Buffered() == 0 {
		return KeyEvent{Key: KeyRune, Rune: 0x1b}, nil
	}
	second, err := r.ReadByte()
	if err != nil {
		if err == io.EOF {
			return KeyEvent{Key: KeyEOF}, nil
		}
		return KeyEvent{}, err
	}
	if second != '[' {
		return KeyEvent{Key: KeyRune, Rune: 0x1b}, nil
	}

	var params []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				return KeyEvent{Key: KeyEOF}, nil
			}
			return KeyEvent{}, err
		}
		if c >= 0x40 && c <= 0x7e {
			return decodeCSI(c, params), nil
		}
		params = append(params, c)
		if len(params) > 16 {
			// Runaway sequence (shouldn't happen for real terminals) —
			// give up and report unknown rather than reading forever.
			return KeyEvent{Key: KeyUnknown}, nil
		}
	}
}

func decodeCSI(final byte, params []byte) KeyEvent {
	switch final {
	case 'A':
		return KeyEvent{Key: KeyUp}
	case 'B':
		return KeyEvent{Key: KeyDown}
	case 'C':
		return KeyEvent{Key: KeyRight}
	case 'D':
		return KeyEvent{Key: KeyLeft}
	case 'H':
		return KeyEvent{Key: KeyHome}
	case 'F':
		return KeyEvent{Key: KeyEnd}
	case '~':
		switch string(params) {
		case "5":
			return KeyEvent{Key: KeyPageUp}
		case "6":
			return KeyEvent{Key: KeyPageDown}
		case "1", "7":
			return KeyEvent{Key: KeyHome}
		case "4", "8":
			return KeyEvent{Key: KeyEnd}
		}
	}
	return KeyEvent{Key: KeyUnknown}
}

// StartKeyReader runs ReadKey in a loop on a background goroutine and
// publishes decoded events to the returned channel, closing it after a
// KeyEOF or a read error. This lets the main loop use select to multiplex
// key events against resize signals and debounce timers instead of the
// polling-with-timeout style viewer.py needed without goroutines.
func StartKeyReader(r *bufio.Reader) <-chan KeyEvent {
	ch := make(chan KeyEvent, 32)
	go func() {
		defer close(ch)
		for {
			ev, err := ReadKey(r)
			if err != nil {
				return
			}
			ch <- ev
			if ev.Key == KeyEOF {
				return
			}
		}
	}()
	return ch
}
