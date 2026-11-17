package kgp

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestMoveCursorIsOneBased(t *testing.T) {
	if got := MoveCursor(0, 0); got != "\x1b[1;1H" {
		t.Errorf("MoveCursor(0,0) = %q", got)
	}
	if got := MoveCursor(4, 9); got != "\x1b[10;5H" {
		t.Errorf("MoveCursor(4,9) = %q", got)
	}
}

func TestTransmitSingleChunkExactBytes(t *testing.T) {
	got := string(Transmit(7, Placement{GridCols: 30, GridRows: 12}, []byte("abc")))
	want := "\x1b_Ga=T,f=100,t=d,i=7,c=30,r=12,C=1,q=2,m=0;YWJj\x1b\\"
	if got != want {
		t.Errorf("Transmit =\n %q\nwant\n %q", got, want)
	}
}

func TestTransmitChunksAtChunkSize(t *testing.T) {
	// 3072 raw bytes -> exactly 4096 base64 bytes: one full chunk, no spill.
	exact := bytes.Repeat([]byte{0xAB}, 3072)
	got := string(Transmit(1, Placement{GridCols: 1, GridRows: 1}, exact))
	if n := strings.Count(got, "\x1b_G"); n != 1 || !strings.Contains(got, ",m=0;") {
		t.Errorf("exact fit: %d chunks, want 1 with m=0", n)
	}

	// One byte more spills 4 base64 bytes into a second chunk.
	raw := append(exact, 0xCD)
	got = string(Transmit(1, Placement{GridCols: 1, GridRows: 1}, raw))
	b64 := base64.StdEncoding.EncodeToString(raw)
	want := "\x1b_Ga=T,f=100,t=d,i=1,c=1,r=1,C=1,q=2,m=1;" + b64[:4096] + "\x1b\\" +
		"\x1b_Gq=2,m=0;" + b64[4096:] + "\x1b\\"
	if got != want {
		t.Errorf("two-chunk output differs; got %d bytes want %d", len(got), len(want))
	}
}

func TestTransmitChunksReassemble(t *testing.T) {
	raw := make([]byte, 20000)
	for i := range raw {
		raw[i] = byte(i * 31)
	}
	out := string(Transmit(3, Placement{GridCols: 5, GridRows: 5}, raw))
	var b64 strings.Builder
	chunks := strings.Split(strings.TrimSuffix(out, "\x1b\\"), "\x1b\\")
	for i, c := range chunks {
		c = strings.TrimPrefix(c, "\x1b_G")
		keys, payload, ok := strings.Cut(c, ";")
		if !ok {
			t.Fatalf("chunk %d has no payload separator", i)
		}
		if len(payload) > ChunkSize {
			t.Errorf("chunk %d payload is %d bytes, over %d", i, len(payload), ChunkSize)
		}
		last := i == len(chunks)-1
		if last != strings.HasSuffix(keys, "m=0") {
			t.Errorf("chunk %d keys %q: m flag wrong for position", i, keys)
		}
		if !strings.Contains(keys, "q=2") {
			t.Errorf("chunk %d lacks q=2: %q", i, keys)
		}
		b64.WriteString(payload)
	}
	dec, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil || !bytes.Equal(dec, raw) {
		t.Errorf("chunks do not reassemble to the input (err=%v)", err)
	}
}

func TestTransmitEmptyPayload(t *testing.T) {
	got := string(Transmit(2, Placement{GridCols: 1, GridRows: 1}, nil))
	if want := "\x1b_Ga=T,f=100,t=d,i=2,c=1,r=1,C=1,q=2,m=0;\x1b\\"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestDrawMovesCursorFirst(t *testing.T) {
	got := string(Draw(9, Placement{ViewportCol: 2, ViewportRow: 3, GridCols: 4, GridRows: 5}, []byte("abc")))
	want := "\x1b[4;3H\x1b_Ga=T,f=100,t=d,i=9,c=4,r=5,C=1,q=2,m=0;YWJj\x1b\\"
	if got != want {
		t.Errorf("Draw = %q, want %q", got, want)
	}
}

func TestDeleteSequences(t *testing.T) {
	if got := string(Delete(12)); got != "\x1b_Ga=d,d=I,i=12,q=2\x1b\\" {
		t.Errorf("Delete = %q", got)
	}
	if got := string(DeleteAll()); got != "\x1b_Ga=d,d=A,q=2\x1b\\" {
		t.Errorf("DeleteAll = %q", got)
	}
}
