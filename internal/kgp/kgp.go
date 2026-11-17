// Package kgp builds Kitty graphics protocol escapes. It is pure: nothing
// here touches a terminal, so every byte can be asserted in tests.
//
// Every command carries q=2, because herdr otherwise answers each one on the
// pane's stdin, where the viewer reads its keys.
package kgp

import (
	"encoding/base64"
	"strconv"
)

const (
	apcStart = "\x1b_G"
	apcEnd   = "\x1b\\"

	// ChunkSize is the largest base64 payload one APC sequence may carry.
	ChunkSize = 4096
)

// Placement is the cell box an image is drawn into, 0-based from the pane's
// top-left corner.
type Placement struct {
	ViewportCol int
	ViewportRow int
	GridCols    int
	GridRows    int
}

// MoveCursor returns the CSI that parks the cursor on a 0-based cell. Images
// are placed at the cursor.
func MoveCursor(col, row int) string {
	return "\x1b[" + strconv.Itoa(row+1) + ";" + strconv.Itoa(col+1) + "H"
}

// Transmit returns the chunked "transmit and place" sequence for one PNG.
// c/r make herdr scale the image to the cell box, and C=1 keeps the cursor
// still so text drawn afterwards is not displaced.
func Transmit(id uint32, p Placement, png []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(png)
	out := make([]byte, 0, len(b64)+len(b64)/ChunkSize*16+128)
	first := true
	for {
		n := len(b64)
		more := 0
		if n > ChunkSize {
			n, more = ChunkSize, 1
		}
		out = append(out, apcStart...)
		if first {
			out = append(out, "a=T,f=100,t=d,i="...)
			out = strconv.AppendUint(out, uint64(id), 10)
			out = append(out, ",c="...)
			out = strconv.AppendInt(out, int64(p.GridCols), 10)
			out = append(out, ",r="...)
			out = strconv.AppendInt(out, int64(p.GridRows), 10)
			out = append(out, ",C=1,q=2,"...)
			first = false
		} else {
			out = append(out, "q=2,"...)
		}
		out = append(out, "m="...)
		out = strconv.AppendInt(out, int64(more), 10)
		out = append(out, ';')
		out = append(out, b64[:n]...)
		out = append(out, apcEnd...)
		b64 = b64[n:]
		if more == 0 {
			return out
		}
	}
}

// Draw moves the cursor to the placement and transmits the image there.
func Draw(id uint32, p Placement, png []byte) []byte {
	out := []byte(MoveCursor(p.ViewportCol, p.ViewportRow))
	return append(out, Transmit(id, p, png)...)
}

// Delete frees one image and its data. The uppercase d=I is what releases the
// storage; herdr silently refuses new images once a pane's quota is full.
func Delete(id uint32) []byte {
	return []byte(apcStart + "a=d,d=I,i=" + strconv.FormatUint(uint64(id), 10) + ",q=2" + apcEnd)
}

// DeleteAll frees every image in the pane.
func DeleteAll() []byte {
	return []byte(apcStart + "a=d,d=A,q=2" + apcEnd)
}
