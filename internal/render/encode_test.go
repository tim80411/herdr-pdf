package render

import (
	"bytes"
	"errors"
	"image/png"
	"testing"
)

// textPage approximates what a rendered PDF text page looks like to a PNG
// encoder: mostly white, with dark runs where the glyphs are, and a faint
// colour tint so the greyscale rung has something to actually discard.
func textPage(w, h int) *Frame {
	pix := make([]byte, w*h*4)
	for i := range pix {
		pix[i] = 0xff
	}
	rnd := uint32(12345)
	next := func() uint32 { rnd ^= rnd << 13; rnd ^= rnd >> 17; rnd ^= rnd << 5; return rnd }
	for y := 0; y < h; y++ {
		if (y/6)%3 == 0 {
			continue
		}
		for x := 0; x < w; x++ {
			if next()%3 == 0 {
				o := (y*w + x) * 4
				pix[o], pix[o+1], pix[o+2] = 0x20, 0x28, 0x40
			}
		}
	}
	return &Frame{Width: w, Height: h, RGBA: pix}
}

func TestEncodeAdaptiveStepsDownUntilItFits(t *testing.T) {
	frame := textPage(400, 560)

	full, err := EncodeAdaptive(frame, 0)
	if err != nil {
		t.Fatalf("unbudgeted encode: %v", err)
	}
	if full.Strategy != "rgb" {
		t.Errorf("no budget should keep truecolour, got %q", full.Strategy)
	}

	// Each tighter budget must produce something no larger than the budget
	// (or, at worst, smaller than the rung above it) and still decode.
	prev := len(full.Payload)
	for _, budget := range []int{len(full.Payload) - 1, len(full.Payload) / 3, len(full.Payload) / 12} {
		enc, err := EncodeAdaptive(frame, budget)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if len(enc.Payload) > prev {
			t.Errorf("budget %d produced %d bytes, larger than the %d from the looser budget",
				budget, len(enc.Payload), prev)
		}
		img, derr := png.Decode(bytes.NewReader(enc.Payload))
		if derr != nil {
			t.Fatalf("budget %d produced undecodable PNG: %v", budget, derr)
		}
		// A rung that scales down must report the size it actually encoded,
		// because the caller recomputes the pane placement from it.
		if got := img.Bounds(); got.Dx() != enc.Width || got.Dy() != enc.Height {
			t.Errorf("budget %d: reported %dx%d but PNG is %dx%d",
				budget, enc.Width, enc.Height, got.Dx(), got.Dy())
		}
		t.Logf("budget=%d -> strategy=%s bytes=%d size=%dx%d", budget, enc.Strategy, len(enc.Payload), enc.Width, enc.Height)
		prev = len(enc.Payload)
	}
}

func TestEncodeAdaptiveNeverFailsOnImpossibleBudget(t *testing.T) {
	// One byte is unreachable. The viewer must still get a drawable frame
	// back rather than an error, which would blank the pane.
	enc, err := EncodeAdaptive(textPage(200, 280), 1)
	if err != nil {
		t.Fatalf("impossible budget returned an error: %v", err)
	}
	if len(enc.Payload) == 0 {
		t.Fatal("impossible budget returned an empty payload")
	}
	if _, derr := png.Decode(bytes.NewReader(enc.Payload)); derr != nil {
		t.Fatalf("impossible budget produced undecodable PNG: %v", derr)
	}
}

func TestEncodeAdaptiveLimitedShrinksPastTheLadderToFit(t *testing.T) {
	frame := textPage(400, 560)

	full, err := EncodeAdaptive(frame, 0)
	if err != nil {
		t.Fatalf("unbudgeted encode: %v", err)
	}
	// No soft budget, so the ladder returns truecolour; the hard ceiling
	// below it must still force something smaller out.
	hardMax := len(full.Payload) / 4
	enc, err := EncodeAdaptiveLimited(frame, 0, hardMax)
	if err != nil {
		t.Fatalf("EncodeAdaptiveLimited: %v", err)
	}
	if len(enc.Payload) > hardMax {
		t.Errorf("returned %d bytes, over the %d hard limit", len(enc.Payload), hardMax)
	}
	img, derr := png.Decode(bytes.NewReader(enc.Payload))
	if derr != nil {
		t.Fatalf("undecodable PNG: %v", derr)
	}
	if got := img.Bounds(); got.Dx() != enc.Width || got.Dy() != enc.Height {
		t.Errorf("reported %dx%d but PNG is %dx%d", enc.Width, enc.Height, got.Dx(), got.Dy())
	}
}

func TestEncodeAdaptiveLimitedReportsAnUnreachableCeiling(t *testing.T) {
	// EncodeAdaptive alone returns an over-budget payload here; the limited
	// form must refuse instead of handing back bytes the stream would drop.
	_, err := EncodeAdaptiveLimited(textPage(400, 560), 1, 1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestEncodeAdaptiveLimitedPassesThroughWhenItAlreadyFits(t *testing.T) {
	frame := textPage(200, 280)
	enc, err := EncodeAdaptiveLimited(frame, 0, 1<<24)
	if err != nil {
		t.Fatalf("EncodeAdaptiveLimited: %v", err)
	}
	if enc.Strategy != "rgb" {
		t.Errorf("strategy = %q, want the ladder's untouched result", enc.Strategy)
	}
	if enc.Width != 200 || enc.Height != 280 {
		t.Errorf("size = %dx%d, want 200x280", enc.Width, enc.Height)
	}
}
