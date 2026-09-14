package viewer

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/tim80411/herdr-pdf/internal/render"
	"github.com/tim80411/herdr-pdf/internal/ui"
)

func keyRune(r rune) ui.KeyEvent {
	return ui.KeyEvent{Key: ui.KeyRune, Rune: r}
}

func TestCeilDiv(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{1061, 38, 28}, // G1-report's own measured example: 1061/38 = 27.9 -> 28
		{750, 15, 50},
		{0, 15, 1},
		{15, 15, 1},
		{16, 15, 2},
	}
	for _, c := range cases {
		if got := ceilDiv(c.a, c.b); got != c.want {
			t.Errorf("ceilDiv(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestComputePlacementCentersAndUsesCeil(t *testing.T) {
	// From G1-report's real measurement: cell=15x38, canvas image 750x1061,
	// pane 50 cols x 55 rows (54 draw rows after status line).
	p := computePlacement(50, 54, 15, 38, 750, 1061)
	if p.GridCols != 50 {
		t.Errorf("GridCols = %d, want 50", p.GridCols)
	}
	if p.GridRows != 28 {
		t.Errorf("GridRows = %d, want 28", p.GridRows)
	}
	if p.ViewportCol != 0 {
		t.Errorf("ViewportCol = %d, want 0 (image fills full width)", p.ViewportCol)
	}
	wantRowOffset := (54 - 28) / 2
	if p.ViewportRow != wantRowOffset {
		t.Errorf("ViewportRow = %d, want %d", p.ViewportRow, wantRowOffset)
	}
}

func TestComputePlacementNeverNegativeOffset(t *testing.T) {
	// A grid larger than the pane (shouldn't normally happen since
	// RenderPage fits within the canvas, but must not go negative).
	p := computePlacement(10, 10, 15, 38, 100000, 100000)
	if p.ViewportCol < 0 || p.ViewportRow < 0 {
		t.Errorf("negative offset: %+v", p)
	}
}

func TestPlanShrinkTerminatesAfterMaxTries(t *testing.T) {
	// This is the direct fix for ARCH-review's P0 finding: viewer.py's
	// degrade loop ran a fixed 8 rounds and then unconditionally returned
	// whatever it had, even if still 3x over budget. planShrink must
	// report ok=false once the attempt budget is exhausted, so the caller
	// can give up and send nothing instead.
	scale := 1.0
	shrinks := 0
	for attempt := 0; attempt < setMaxTries; attempt++ {
		var ok bool
		var w, h int
		scale, w, h, ok = planShrink(attempt, 1000, 1000, scale)
		if !ok {
			break
		}
		shrinks++
		if w <= 0 || h <= 0 {
			t.Fatalf("attempt %d: non-positive dimensions %dx%d", attempt, w, h)
		}
	}
	if shrinks != setMaxTries-1 {
		t.Errorf("got %d shrink steps, want exactly %d (setMaxTries-1)", shrinks, setMaxTries-1)
	}
	// One more call past the budget must still say no.
	if _, _, _, ok := planShrink(setMaxTries-1, 1000, 1000, scale); ok {
		t.Errorf("planShrink at attempt=setMaxTries-1 should refuse, got ok=true")
	}
	if _, _, _, ok := planShrink(setMaxTries, 1000, 1000, scale); ok {
		t.Errorf("planShrink at attempt=setMaxTries should refuse, got ok=true")
	}
}

func TestPlanShrinkMonotonicallyShrinks(t *testing.T) {
	scale := 1.0
	prevW, prevH := 1000, 1000
	for attempt := 0; attempt < setMaxTries-1; attempt++ {
		var w, h int
		var ok bool
		scale, w, h, ok = planShrink(attempt, 1000, 1000, scale)
		if !ok {
			t.Fatalf("attempt %d: expected ok=true", attempt)
		}
		if w >= prevW || h >= prevH {
			t.Errorf("attempt %d: %dx%d did not shrink from %dx%d", attempt, w, h, prevW, prevH)
		}
		prevW, prevH = w, h
	}
}

func TestPlanShrinkNeverZero(t *testing.T) {
	// Even a tiny base size must never shrink to 0 (which would make an
	// invalid render request).
	if _, w, h, ok := planShrink(0, 1, 1, 1.0); ok && (w <= 0 || h <= 0) {
		t.Errorf("planShrink produced non-positive size %dx%d", w, h)
	}
}

func TestEncodeAdaptiveRoundTrips(t *testing.T) {
	w, h := 4, 3
	pix := make([]byte, w*h*4)
	for i := range pix {
		pix[i] = byte(i % 256)
	}
	frame := &render.Frame{Width: w, Height: h, RGBA: pix}

	enc, err := render.EncodeAdaptive(frame, 0)
	if err != nil {
		t.Fatalf("EncodeAdaptive: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(enc.Payload))
	if err != nil {
		t.Fatalf("decode produced PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != w || b.Dy() != h {
		t.Errorf("decoded size = %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}
	if enc.Width != w || enc.Height != h {
		t.Errorf("reported size = %dx%d, want %dx%d", enc.Width, enc.Height, w, h)
	}
}
func TestHandleKeyNumericPrefixAndG(t *testing.T) {
	// Build a Viewer with a fake renderer via the public surface is
	// awkward without a real PDF; this test exercises the numeric-buffer
	// state machine directly through confirmJump/jumpOrLast semantics that
	// don't require v.renderer, by keeping the buffer manipulations
	// observable through v.numBuf and v.needRedraw before a renderer is
	// touched (digit accumulation only).
	v := &Viewer{}
	for _, r := range "12" {
		v.handleKey(keyRune(r))
	}
	if v.numBuf != "12" {
		t.Errorf("numBuf = %q, want %q", v.numBuf, "12")
	}
	// A non-digit, non-jump key clears the pending buffer.
	v.handleKey(keyRune('x'))
	if v.numBuf != "" {
		t.Errorf("numBuf = %q, want empty after non-digit key", v.numBuf)
	}
}

func TestParseTransport(t *testing.T) {
	// An unrecognised or empty value must not disable rendering; it falls
	// back to the default so a typo in $HERDR_PDF_TRANSPORT degrades to
	// "not the transport you asked for" rather than a blank pane.
	for in, want := range map[string]Transport{
		"stream-png":  TransportStreamPNG,
		"stream-rgba": TransportStreamRGBA,
		"  DIRECT  ":  TransportDirect,
		"Set":         TransportSet,
		"":            TransportStreamPNG,
		"nonsense":    TransportStreamPNG,
	} {
		if got := ParseTransport(in); got != want {
			t.Errorf("ParseTransport(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPNGCacheEvictsOldestAndKeysOnSize(t *testing.T) {
	v := &Viewer{}
	frame := func(w, h int) *render.Frame { return &render.Frame{Width: w, Height: h} }

	for p := 0; p < pngCacheCap+2; p++ {
		v.storePNGAt(p, frame(100, 200), encodedPage{full: render.Encoded{Payload: []byte{byte(p)}}})
	}
	if len(v.pngCache) != pngCacheCap {
		t.Fatalf("cache holds %d entries, want cap %d", len(v.pngCache), pngCacheCap)
	}
	if _, ok := v.pngCache[pngKey{0, 100, 200}]; ok {
		t.Error("oldest entry should have been evicted")
	}

	// A resize changes the frame dimensions, so the same page number must
	// not serve the encode made for the previous size.
	v.page = pngCacheCap + 1
	if _, ok := v.lookupPNG(frame(100, 200)); !ok {
		t.Fatal("expected a hit at the stored size")
	}
	if _, ok := v.lookupPNG(frame(50, 100)); ok {
		t.Error("a different frame size must miss, not serve a stale encode")
	}
}
