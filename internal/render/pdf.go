// Package render rasterizes PDF pages to tightly-packed RGBA using
// go-pdfium's WebAssembly (wazero) backend — no system Poppler/MuPDF dependency.
package render

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pdfiumtop "github.com/klippa-app/go-pdfium"
	pdfiumerr "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// Frame is one rendered page: exactly Width*Height*4 bytes of straight RGBA
// with no row padding, ready to encode and send.
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
// pdfium is single-threaded here (one wazero instance, MaxTotal 1), so every
// call is serialised by renderMu — this is what makes it safe for a
// background prefetch goroutine to run alongside the main loop.
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

	// inflight lets a second request for a key wait on the render already
	// running for it instead of rasterising the same page twice. Guarded by
	// cacheMu so the cache lookup and the claim are one atomic decision.
	inflight map[cacheKey]chan struct{}
}

// WazeroCacheDir returns where to persist wazero's compiled-module cache, so
// pdfium's cold compile happens once per machine rather than once per pane
// launch. Prefers $HERDR_PLUGIN_STATE_DIR/wazero, falling back to
// ~/.cache/herdr-pdf/wazero outside herdr.
func WazeroCacheDir() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return filepath.Join(d, "wazero")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "herdr-pdf", "wazero")
}

// NewRenderer starts the pdfium WebAssembly runtime, warming the compile
// cache at cacheDir on first run. Run this before the terminal enters raw
// mode, so a cold compile doesn't look like a silent hang with no cursor.
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

// Open failure classes worth telling the reader apart.
var (
	// ErrEncrypted reports a document that needs a password to open.
	ErrEncrypted = errors.New("pdf is password protected")
	// ErrCorrupt reports bytes pdfium cannot parse as a PDF.
	ErrCorrupt = errors.New("pdf is corrupt or not a pdf")
)

// classifyOpenError tags pdfium's open failures, keeping the library's own
// message for the log. Sentinels are matched first; the message text is a
// fallback for a backend that reports the failure some other way.
func classifyOpenError(err error) error {
	switch {
	case errors.Is(err, pdfiumerr.ErrPassword), errors.Is(err, pdfiumerr.ErrSecurity):
		return fmt.Errorf("%w: %v", ErrEncrypted, err)
	case errors.Is(err, pdfiumerr.ErrFormat):
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "password"), strings.Contains(msg, "invalid encryption"):
		return fmt.Errorf("%w: %v", ErrEncrypted, err)
	case strings.Contains(msg, "incorrect format"), strings.Contains(msg, "format error"):
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return err
}

// Open loads a PDF file. Call once per Renderer, before RenderPage.
func (r *Renderer) Open(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	// Reopening is how browser mode switches files within one process. The
	// previous document has to be released and its rasters dropped: the
	// cache is keyed by page index and size, not by file, so a survivor
	// would be served as a page of the new document.
	r.closeDoc()
	r.resetCache()

	r.pdfBytes = data
	doc, err := r.inst.OpenDocument(&requests.OpenDocument{File: &r.pdfBytes})
	if err != nil {
		return classifyOpenError(err)
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

// closeDoc releases the open document, if there is one. It takes renderMu
// so a prefetch still inside pdfium finishes before the document goes away.
func (r *Renderer) closeDoc() {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	if r.doc == nil {
		return
	}
	r.inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: *r.doc})
	r.doc = nil
	r.pageCount = 0
}

// resetCache drops every cached raster. storeLocked recreates the map, so a
// render already in flight can still store its result.
func (r *Renderer) resetCache() {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.cache = nil
	r.cacheKeys = nil
}

// PageCount returns the number of pages in the open document.
func (r *Renderer) PageCount() int { return r.pageCount }

// RenderPage renders page index (0-based), fit within a maxW x maxH pixel
// bounding box; RenderPageInPixels treats width/height as a maximum and
// preserves aspect ratio internally. Results are cached by (page, maxW,
// maxH), so paging back and forth at an unchanged canvas size is free.
func (r *Renderer) RenderPage(index, maxW, maxH int) (*Frame, error) {
	if index < 0 || index >= r.pageCount {
		return nil, fmt.Errorf("page %d out of range [0,%d)", index, r.pageCount)
	}
	if maxW <= 0 || maxH <= 0 {
		return nil, fmt.Errorf("invalid target size %dx%d", maxW, maxH)
	}
	key := cacheKey{page: index, w: maxW, h: maxH}

	return r.renderOnce(key, func() (*Frame, error) {
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
		return &Frame{Width: w, Height: h, RGBA: pix}, nil
	})
}

// renderOnce serves key from the cache, or runs render for it while any
// other caller for the same key waits and then re-reads the cache. A failed
// render leaves nothing cached, so the next caller retries it.
func (r *Renderer) renderOnce(key cacheKey, render func() (*Frame, error)) (*Frame, error) {
	for {
		r.cacheMu.Lock()
		if f, ok := r.cache[key]; ok {
			r.cacheMu.Unlock()
			return f, nil
		}
		if wait, ok := r.inflight[key]; ok {
			r.cacheMu.Unlock()
			<-wait
			continue
		}
		if r.inflight == nil {
			r.inflight = make(map[cacheKey]chan struct{})
		}
		done := make(chan struct{})
		r.inflight[key] = done
		r.cacheMu.Unlock()

		frame, err := render()

		r.cacheMu.Lock()
		if err == nil {
			r.storeLocked(key, frame)
		}
		delete(r.inflight, key)
		r.cacheMu.Unlock()
		close(done)
		return frame, err
	}
}

// storeLocked adds a frame and evicts the oldest past cacheCap. Callers hold cacheMu.
func (r *Renderer) storeLocked(key cacheKey, frame *Frame) {
	if r.cache == nil {
		r.cache = make(map[cacheKey]*Frame)
	}
	r.cache[key] = frame
	r.cacheKeys = append(r.cacheKeys, key)
	for len(r.cacheKeys) > r.cacheCap {
		oldest := r.cacheKeys[0]
		r.cacheKeys = r.cacheKeys[1:]
		delete(r.cache, oldest)
	}
}

// Cached reports whether a page is already rendered at this exact size, so
// the caller can skip spawning a prefetch that would do nothing.
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
	r.closeDoc()
	r.renderMu.Lock()
	r.closed = true
	r.renderMu.Unlock()
	if r.inst != nil {
		r.inst.Close()
	}
	if r.pool != nil {
		return r.pool.Close()
	}
	return nil
}

// TightRGBA copies any image into a densely packed w*h*4 straight-RGBA
// buffer. go-pdfium's WebAssembly backend often returns one with row
// padding (Stride != 4*width), but herdr's frame protocol requires exactly
// width*height*4 bytes with none.
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
