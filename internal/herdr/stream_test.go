package herdr

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestSendInlineRejectsFrameOverTheStreamLimit(t *testing.T) {
	s := &StreamSession{}
	err := s.SendInline("png", make([]byte, MaxInlineFrameBytes+1), 100, 100, Placement{})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	// Refusing must happen before the frame counter moves, so a rejected
	// frame does not leave a gap in the sequence herdr sees.
	if s.frameNo != 0 {
		t.Errorf("frameNo = %d, want 0 for a frame that was never written", s.frameNo)
	}
}

func TestSendInlineAcceptsFrameAtTheLimit(t *testing.T) {
	// At exactly the limit the size check must pass it through; the write
	// then fails on the nil connection, which is a different error.
	s := &StreamSession{}
	defer func() { recover() }()
	err := s.SendInline("png", make([]byte, MaxInlineFrameBytes), 100, 100, Placement{})
	if errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("a frame exactly at the limit was rejected: %v", err)
	}
}

func TestSendFrameRejectsOversizedRGBA(t *testing.T) {
	s := &StreamSession{}
	w, h := 4096, 4096 // 4096*4096*4 = 64 MiB
	err := s.SendFrame(make([]byte, w*h*4), w, h, Placement{})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadLineRefusesAnUnboundedResponse(t *testing.T) {
	huge := strings.Repeat("x", maxResponseBytes+1024) + "\n"
	if _, err := readLine(bufio.NewReaderSize(strings.NewReader(huge), 4096)); err == nil {
		t.Fatal("expected an over-long response line to be refused")
	}
}

func TestReadLineReturnsOneLineWithoutTerminator(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("{\"id\":\"a\"}\n{\"id\":\"b\"}\n"), 4096)
	first, err := readLine(br)
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if first != `{"id":"a"}` {
		t.Errorf("got %q, want the first line without its newline", first)
	}
	second, err := readLine(br)
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if second != `{"id":"b"}` {
		t.Errorf("got %q, want the second line", second)
	}
}

func TestReadLineHandlesALineLongerThanTheBuffer(t *testing.T) {
	long := strings.Repeat("y", 40000)
	br := bufio.NewReaderSize(strings.NewReader(long+"\n"), 4096)
	got, err := readLine(br)
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != long {
		t.Errorf("got %d bytes, want %d", len(got), len(long))
	}
}
