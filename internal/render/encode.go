package render

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"
)

// ErrTooLarge reports that no rung of the ladder got the page under the
// transport's hard ceiling, so there is nothing safe to send.
var ErrTooLarge = errors.New("page does not fit the frame size limit")

// Encoded is one frame compressed and ready for herdr's stream API, along
// with the dimensions it ended up at (a scaled-down strategy changes them).
type Encoded struct {
	Payload  []byte
	Strategy string
	Width    int
	Height   int

	// BaseBytes is the truecolour encode size, logged alongside the chosen
	// rung so the step-down's saving is auditable per page.
	BaseBytes int
}

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
// depth (truecolour -> 8-bit grey -> 4-bit palette) and then resolution
// until it fits maxBytes. It never fails for being too big — the smallest
// rung found is returned instead — and maxBytes <= 0 disables the ladder.
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

// EncodeAdaptiveLimited runs the ladder against a soft budget, then keeps
// halving past its last rung until the payload is under hardMax. It reports
// ErrTooLarge rather than handing back bytes the transport would reject.
func EncodeAdaptiveLimited(frame *Frame, maxBytes, hardMax int) (Encoded, error) {
	enc, err := EncodeAdaptive(frame, maxBytes)
	if err != nil {
		return Encoded{}, err
	}
	if hardMax <= 0 || len(enc.Payload) <= hardMax {
		return enc, nil
	}

	gray := toGray(frame)
	for scale := shrinkSteps[len(shrinkSteps)-1] * 0.5; ; scale *= 0.5 {
		small := downsampleGray(gray, scale)
		if small.Bounds().Dx() < 16 || small.Bounds().Dy() < 16 {
			break
		}
		b, gerr := encodeOne(toGray16(small))
		if gerr != nil {
			break
		}
		if len(b) <= hardMax {
			return Encoded{
				Payload:   b,
				Strategy:  fmt.Sprintf("gray16@%d", int(scale*100)),
				Width:     small.Bounds().Dx(),
				Height:    small.Bounds().Dy(),
				BaseBytes: enc.BaseBytes,
			}, nil
		}
	}
	return Encoded{}, fmt.Errorf("%w: smallest encode is %d bytes, limit is %d", ErrTooLarge, len(enc.Payload), hardMax)
}

// toGray flattens to luma using Rec. 601 weights, done inline over the
// packed buffer to avoid three million interface calls at 2.8 megapixels.
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

// downsampleGray box-averages to scale rather than using nearest-neighbour:
// dropping antialiased glyph-edge pixels is what breaks downscaled text.
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

// previewScales start well below the shrink ladder's range: a preview only
// needs to visibly change on keypress, not stay readable.
var previewScales = []float64{0.45, 0.34, 0.25, 0.18}

// EncodePreview produces a deliberately small stand-in for a page —
// 16-level greyscale, scaled down until it fits maxBytes — so a slow link
// gets something on the first render tick instead of the frame being
// dropped by herdr's single-frame channel and retried later.
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
