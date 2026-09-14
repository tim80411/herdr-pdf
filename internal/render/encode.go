package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"
)

// Encoded is one frame compressed and ready to hand to herdr's stream API,
// together with the dimensions it actually ended up at — a strategy that
// scales the page down changes them, and the caller has to recompute the
// pane placement from these rather than from the source frame.
type Encoded struct {
	Payload  []byte
	Strategy string
	Width    int
	Height   int

	// BaseBytes is what the truecolour encode of the same page came to.
	// Logging it next to the chosen rung is what makes the ladder
	// auditable: it says how many bytes the step-down actually saved on
	// this page, rather than leaving it to be inferred from a second run
	// at a different pane size.
	BaseBytes int
}

// Bytes returned by the ladder below are what crosses the link to the
// user's terminal, and on a remote session that link is the whole problem:
// herdr compresses nothing anywhere between plugin and terminal (no zlib on
// the Kitty path, no compression crate in its dependencies, and it invokes
// ssh without -C), and a graphics payload is serialised into the *same*
// bincode message as the pane's text surface (protocol/wire.rs:1213-1222),
// so an oversized page delays the text update riding with it. The
// server→client render channel then holds exactly one frame and drops
// rather than queues when the link is busy (server/client_transport.rs:119-124,
// "Capacity is one so slow clients cannot build lag"), so a frame that
// arrives while the previous one is still going out is discarded and
// retried on the next tick. Fewer bytes is therefore not just less transfer
// time, it is fewer missed ticks.

type pngBufferPool struct{ p sync.Pool }

func (b *pngBufferPool) Get() *png.EncoderBuffer {
	eb, _ := b.p.Get().(*png.EncoderBuffer)
	return eb
}

func (b *pngBufferPool) Put(eb *png.EncoderBuffer) { b.p.Put(eb) }

// Reusing one flate compressor across frames matters here because the
// ladder can encode the same page up to three times looking for a fit.
var sharedEncoder = png.Encoder{
	CompressionLevel: png.BestSpeed,
	BufferPool:       &pngBufferPool{},
}

func encodeOne(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(256 << 10)
	if err := sharedEncoder.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// shrinkSteps are the scale factors tried after colour depth has already
// been spent. Downscaling is last because it is the only rung that costs
// legibility on a text page; dropping to greyscale and then to 16 levels is
// close to free on a document that is mostly black ink on white paper.
var shrinkSteps = []float64{0.85, 0.72, 0.61, 0.5}

// EncodeAdaptive compresses frame as PNG, stepping down through colour
// depth and then resolution until the result fits maxBytes.
//
// The rungs, in order: truecolour (Go writes an opaque RGBA image as 8-bit
// RGB, so this is already 3 bytes per pixel, not 4), 8-bit greyscale,
// 16-level greyscale (which image/png emits as a 4-bit palette, half the
// raw size again), then that same 4-bit form progressively scaled down.
//
// It never fails for being too big: if even the smallest rung overshoots,
// the smallest result found is returned anyway, because a slightly
// oversized page still draws and a returned error would blank the pane.
// maxBytes <= 0 disables the ladder and returns the truecolour encode.
func EncodeAdaptive(frame *Frame, maxBytes int) (Encoded, error) {
	rgba := &image.RGBA{
		Pix:    frame.RGBA,
		Stride: frame.Width * 4,
		Rect:   image.Rect(0, 0, frame.Width, frame.Height),
	}

	full, err := encodeOne(rgba)
	if err != nil {
		return Encoded{}, fmt.Errorf("encode rgb: %w", err)
	}
	best := Encoded{Payload: full, Strategy: "rgb", Width: frame.Width, Height: frame.Height, BaseBytes: len(full)}
	if maxBytes <= 0 || len(full) <= maxBytes {
		return best, nil
	}

	gray := toGray(frame)
	if b, gerr := encodeOne(gray); gerr == nil {
		cand := Encoded{Payload: b, Strategy: "gray", Width: frame.Width, Height: frame.Height, BaseBytes: len(full)}
		if len(b) < len(best.Payload) {
			best = cand
		}
		if len(b) <= maxBytes {
			return cand, nil
		}
	}

	if b, gerr := encodeOne(toGray16(gray)); gerr == nil {
		cand := Encoded{Payload: b, Strategy: "gray16", Width: frame.Width, Height: frame.Height, BaseBytes: len(full)}
		if len(b) < len(best.Payload) {
			best = cand
		}
		if len(b) <= maxBytes {
			return cand, nil
		}
	}

	for _, scale := range shrinkSteps {
		small := downsampleGray(gray, scale)
		if small.Bounds().Dx() < 16 || small.Bounds().Dy() < 16 {
			break
		}
		b, gerr := encodeOne(toGray16(small))
		if gerr != nil {
			continue
		}
		cand := Encoded{
			Payload:   b,
			Strategy:  fmt.Sprintf("gray16@%d", int(scale*100)),
			Width:     small.Bounds().Dx(),
			Height:    small.Bounds().Dy(),
			BaseBytes: len(full),
		}
		if len(b) < len(best.Payload) {
			best = cand
		}
		if len(b) <= maxBytes {
			return cand, nil
		}
	}

	return best, nil
}

// toGray flattens to luma with the Rec. 601 weights image/color uses, done
// inline over the packed buffer rather than through color.Model conversion
// so a 2.8 megapixel page costs one linear pass instead of three million
// interface calls.
func toGray(frame *Frame) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, frame.Width, frame.Height))
	src := frame.RGBA
	for i, j := 0, 0; j < len(g.Pix); i, j = i+4, j+1 {
		r := uint32(src[i])
		gr := uint32(src[i+1])
		b := uint32(src[i+2])
		g.Pix[j] = uint8((19595*r + 38470*gr + 7471*b + 1<<15) >> 16)
	}
	return g
}

// gray16Palette is 16 evenly spaced levels from black to white. image/png
// writes a palette this size as 4 bits per pixel, so the raw image data is
// half what 8-bit greyscale needs before compression even starts.
var gray16Palette = func() color.Palette {
	p := make(color.Palette, 16)
	for i := range p {
		v := uint8(i * 17) // 0, 17, 34, ... 255
		p[i] = color.RGBA{v, v, v, 0xff}
	}
	return p
}()

func toGray16(g *image.Gray) *image.Paletted {
	b := g.Bounds()
	p := image.NewPaletted(b, gray16Palette)
	for i, v := range g.Pix {
		p.Pix[i] = v >> 4
	}
	return p
}

// downsampleGray box-averages to scale. A box filter rather than nearest
// neighbour because dropping pixels from antialiased glyph edges is what
// makes downscaled text look broken; averaging keeps the stroke weight.
func downsampleGray(src *image.Gray, scale float64) *image.Gray {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := int(float64(sw)*scale), int(float64(sh)*scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewGray(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0 := y * sh / dh
		y1 := (y + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := x * sw / dw
			x1 := (x + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum, n uint32
			for yy := y0; yy < y1; yy++ {
				row := yy * src.Stride
				for xx := x0; xx < x1; xx++ {
					sum += uint32(src.Pix[row+xx])
					n++
				}
			}
			dst.Pix[y*dst.Stride+x] = uint8(sum / n)
		}
	}
	return dst
}

// previewScales are tried in order until one fits the preview budget. They
// start well below the shrink ladder's range because a preview is not
// trying to stay readable — it only has to make the page visibly change at
// the instant the key is pressed, with the sharp version arriving behind it.
var previewScales = []float64{0.45, 0.34, 0.25, 0.18}

// EncodePreview produces a deliberately small stand-in for a page: 16-level
// greyscale, scaled down until it fits maxBytes.
//
// It exists because of how herdr delivers frames to a client. The
// server→client render channel holds exactly one message and drops instead
// of queueing when it is busy (server/client_transport.rs:119-124), so on a
// slow link the frame produced by a keypress can be discarded and retried
// on a later tick — which is the lag the reader feels. A frame small enough
// to clear the link in one go lands on the first tick; the full-resolution
// frame then replaces it once the link is free again.
func EncodePreview(frame *Frame, maxBytes int) (Encoded, error) {
	gray := toGray(frame)
	var best Encoded
	for _, scale := range previewScales {
		small := downsampleGray(gray, scale)
		if small.Bounds().Dx() < 16 || small.Bounds().Dy() < 16 {
			break
		}
		b, err := encodeOne(toGray16(small))
		if err != nil {
			continue
		}
		cand := Encoded{
			Payload:  b,
			Strategy: fmt.Sprintf("preview@%d", int(scale*100)),
			Width:    small.Bounds().Dx(),
			Height:   small.Bounds().Dy(),
		}
		if best.Payload == nil || len(b) < len(best.Payload) {
			best = cand
		}
		if maxBytes <= 0 || len(b) <= maxBytes {
			return cand, nil
		}
	}
	if best.Payload == nil {
		return Encoded{}, fmt.Errorf("preview encode produced nothing for %dx%d", frame.Width, frame.Height)
	}
	return best, nil
}
