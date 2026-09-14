// Package render rasterizes PDF pages to tightly-packed RGBA using
// go-pdfium's WebAssembly (wazero) backend — no system Poppler/MuPDF
// dependency, matching the license and distribution rationale that picked
// pypdfium2 for the Python version (ARCH-review "設計決策評估").
package render

import (
	"fmt"
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"sync"
	"time"

	pdfiumtop "github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// Frame is one rendered page: exactly Width*Height*4 bytes of straight RGBA
// with no row padding, ready to hand to herdr's stream or set APIs.
type Frame struct {
	Width  int
	Height int
	RGBA   []byte
}

type cacheKey struct {
	page int
	w    int
	h    int
}

// Renderer owns one pdfium WebAssembly instance and one open document.
//
// pdfium itself is single-threaded here (one wazero instance, MaxTotal 1),
// so every call into inst is serialised by renderMu. That lock is what makes
// Prefetch safe: the viewer can warm the next page on a background goroutine
// while the main loop stays free to accept keys, and the two simply queue
// behind each other inside pdfium instead of corrupting it.
type Renderer struct {
	pool pdfiumtop.Pool
	inst pdfiumtop.Pdfium

	// renderMu serialises every inst call, including Close. Without it a
	// Prefetch still in flight when the viewer exits would race the
	// document teardown inside the WebAssembly runtime.
	renderMu sync.Mutex
	closed   bool

	doc      *references.FPDF_DOCUMENT
	pdfBytes []byte // must outlive the open document; pdfium reads from it lazily

	pageCount int

	cacheMu   sync.Mutex
	cacheKeys []cacheKey
	cache     map[cacheKey]*Frame
	cacheCap  int
}

// WazeroCacheDir returns where to persist wazero's compiled-module cache,
// so pdfium's cold compile (~1.3s per G1-report) only ever happens once per
// machine rather than once per pane launch. $HERDR_PLUGIN_STATE_DIR/wazero
// is preferred (it's the plugin's own per-install state directory);
// ~/.cache/herdr-pdf/wazero is the fallback for running outside herdr.
func WazeroCacheDir() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return filepath.Join(d, "wazero")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "herdr-pdf", "wazero")
}

// NewRenderer starts the pdfium WebAssembly runtime using (and warming, if
// this is the first run) the compilation cache at cacheDir. This is the
// expensive step and should run before the terminal enters raw mode, so a
// cold 1.3s compile doesn't look like a silent hang with no cursor.
func NewRenderer(cacheDir string) (*Renderer, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create wazero cache dir %s: %w", cacheDir, err)
	}
	compileCache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("open wazero compilation cache: %w", err)
	}
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle:       1,
		MaxIdle:       1,
		MaxTotal:      1,
		RuntimeConfig: wazero.NewRuntimeConfig().WithCompilationCache(compileCache),
	})
	if err != nil {
		return nil, fmt.Errorf("init pdfium runtime: %w", err)
	}
	inst, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("get pdfium instance: %w", err)
	}
	return &Renderer{
		pool:     pool,
		inst:     inst,
		cache:    make(map[cacheKey]*Frame),
		cacheCap: 6,
	}, nil
}

// Open loads a PDF file. Call once per Renderer, before RenderPage.
func (r *Renderer) Open(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	r.pdfBytes = data
	doc, err := r.inst.OpenDocument(&requests.OpenDocument{File: &r.pdfBytes})
	if err != nil {
		return fmt.Errorf("open document: %w", err)
	}
	r.doc = &doc.Document

	countResp, err := r.inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return fmt.Errorf("get page count: %w", err)
	}
	if countResp.PageCount <= 0 {
		return fmt.Errorf("document has no pages")
	}
	r.pageCount = countResp.PageCount
	return nil
}

// PageCount returns the number of pages in the open document.
func (r *Renderer) PageCount() int { return r.pageCount }

// RenderPage renders page index (0-based), fit within a maxW x maxH pixel
// bounding box. go-pdfium's RenderPageInPixels treats width/height as a
// maximum and preserves aspect ratio internally, so the caller does not
// need to precompute a fit — unlike the Python version, which had to do
// that arithmetic itself against pypdfium2's fixed-scale render() call.
//
// Results are cached by (page, maxW, maxH): a resize naturally produces a
// new cache key rather than needing explicit invalidation, and revisiting a
// page at an unchanged canvas size (the common case: paging back and forth
// without resizing) is free. Cap is small (5) because a PDF viewer only
// ever needs the current page plus its immediate neighbors in flight
// (ARCH-review P1 "沒有頁面快取" — this is that fix, sized generously).
func (r *Renderer) RenderPage(index, maxW, maxH int) (*Frame, error) {
	if index < 0 || index >= r.pageCount {
		return nil, fmt.Errorf("page %d out of range [0,%d)", index, r.pageCount)
	}
	if maxW <= 0 || maxH <= 0 {
		return nil, fmt.Errorf("invalid target size %dx%d", maxW, maxH)
	}
	key := cacheKey{page: index, w: maxW, h: maxH}

	r.cacheMu.Lock()
	if f, ok := r.cache[key]; ok {
		r.cacheMu.Unlock()
		return f, nil
	}
	r.cacheMu.Unlock()

	r.renderMu.Lock()
	if r.closed {
		r.renderMu.Unlock()
		return nil, fmt.Errorf("renderer is closed")
	}
	page := requests.Page{ByIndex: &requests.PageByIndex{Document: *r.doc, Index: index}}
	resp, err := r.inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page: page, Width: maxW, Height: maxH,
	})
	r.renderMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("render page %d: %w", index, err)
	}
	w, h, pix := TightRGBA(resp.Result.RenderedImage)
	// WebAssembly mode: the pixel buffer is only valid until Cleanup() is
	// called. TightRGBA has already copied it out, so it's safe to release now.
	resp.Cleanup()

	frame := &Frame{Width: w, Height: h, RGBA: pix}

	r.cacheMu.Lock()
	r.cache[key] = frame
	r.cacheKeys = append(r.cacheKeys, key)
	for len(r.cacheKeys) > r.cacheCap {
		oldest := r.cacheKeys[0]
		r.cacheKeys = r.cacheKeys[1:]
		delete(r.cache, oldest)
	}
	r.cacheMu.Unlock()

	return frame, nil
}

// Cached reports whether a page is already rendered at this exact size, so
// the caller can skip spawning a prefetch that would do nothing. RenderPage
// itself is the prefetch entry point: it is already cache-backed and
// mutex-guarded, so a background goroutine calling it warms the cache the
// main loop will read.
func (r *Renderer) Cached(index, maxW, maxH int) bool {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	_, ok := r.cache[cacheKey{page: index, w: maxW, h: maxH}]
	return ok
}

// Close releases the pdfium document and the WebAssembly runtime. It blocks
// on renderMu so an in-flight Prefetch finishes before the document under
// it is torn down.
func (r *Renderer) Close() error {
	r.renderMu.Lock()
	r.closed = true
	r.renderMu.Unlock()
	if r.doc != nil {
		r.inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: *r.doc})
	}
	if r.inst != nil {
		r.inst.Close()
	}
	if r.pool != nil {
		return r.pool.Close()
	}
	return nil
}

// TightRGBA copies any image into a densely packed w*h*4 straight-RGBA
// buffer. Go's *image.RGBA does not guarantee Stride == 4*width (and
// go-pdfium's WebAssembly backend often hands back one with row padding),
// but herdr's stream frame protocol requires exactly width*height*4 bytes
// with no padding (G1-report "RGBA stride 一定要自己收緊").
func TightRGBA(img image.Image) (width, height int, pix []byte) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, w*h*4)
	if src, ok := img.(*image.RGBA); ok {
		for y := 0; y < h; y++ {
			srcOff := (y+b.Min.Y-src.Rect.Min.Y)*src.Stride + (b.Min.X-src.Rect.Min.X)*4
			copy(out[y*w*4:(y+1)*w*4], src.Pix[srcOff:srcOff+w*4])
		}
		return w, h, out
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	copy(out, dst.Pix)
	return w, h, out
}
