// Package viewer implements the herdr-pdf "view" pane's state machine:
// open a PDF, take over the pane's terminal, and keep a rendered page
// painted into the pane via herdr's graphics API while reacting to
// keypresses and resizes.
package viewer

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tim80411/herdr-pdf/internal/herdr"
	"github.com/tim80411/herdr-pdf/internal/render"
	"github.com/tim80411/herdr-pdf/internal/ui"
)

// namedLayerID is the graphics layer this viewer owns by default: "page",
// not "primary", so a future feature (search-hit highlighting, an outline
// overlay, ...) can use a second layer without fighting this one for the
// same slot — herdr allows up to 16 layers per pane.
//
// primaryLayerID is the one exception. herdr's zero-copy file transport is
// gated on the layer literally being "primary"
// (app/api/pane_graphics.rs:312-315, PANE_GRAPHICS_PRIMARY_LAYER_ID =
// "primary" at api/schema/panes.rs:11), so TransportDirect has to give up
// the named-layer property to get the fast path.
const (
	namedLayerID   = "page"
	primaryLayerID = "primary"
)

const (
	setMaxBytes  = 500 * 1024 // stay under the 512 KiB hard cap with headroom for base64/JSON overhead
	setMaxTries  = 6
	setShrinkPct = 0.85
)

// Transport selects how a rendered page reaches the terminal. The default
// is TransportStreamPNG; the others exist as escape hatches via
// $HERDR_PDF_TRANSPORT because each one is faster than the default under
// some condition this viewer cannot detect from inside the pane.
type Transport string

const (
	// TransportStreamPNG sends PNG bytes over the long-lived stream. This
	// is the default because herdr never decodes PNG: it stores the
	// compressed bytes as-is and hands Kitty format code 100 to the outer
	// terminal (kitty_graphics.rs:688-694), so compressing the frame
	// shrinks every stage downstream — the owned Vec copy, the
	// whole-buffer hash, the uncompressed bincode wire message, the base64
	// expansion, and the count of 3072-byte Kitty chunks
	// (KITTY_CHUNK_BYTES at kitty_graphics.rs:20).
	TransportStreamPNG Transport = "stream-png"

	// TransportStreamRGBA is the original behaviour: raw RGBA over the
	// stream. No encode cost, but a fit-to-pane frame is ~11 MB, which
	// base64-expands to ~14.7 MB of escape text in ~3,597 chunks on every
	// single page flip (there is no zlib on this path).
	TransportStreamRGBA Transport = "stream-rgba"

	// TransportDirect writes RGBA to a file in pane.graphics.info's
	// file_frame_directory and sends only its path, which lets herdr use
	// Kitty's t=f file transport with no base64 at all. Requires
	// file_frame_transport == "direct-kitty" and forces the layer to
	// "primary"; falls back to TransportStreamPNG when unavailable.
	TransportDirect Transport = "direct"

	// TransportSet is the Python viewer's path: a short-lived
	// pane.graphics.set call per page, PNG, capped at 512 KiB decoded.
	TransportSet Transport = "set"
)

// ParseTransport maps $HERDR_PDF_TRANSPORT onto a Transport, falling back
// to the default for an empty or unrecognised value so a typo degrades to
// "works, just not what you asked for" rather than a dead pane.
func ParseTransport(v string) Transport {
	switch Transport(strings.ToLower(strings.TrimSpace(v))) {
	case TransportStreamPNG:
		return TransportStreamPNG
	case TransportStreamRGBA:
		return TransportStreamRGBA
	case TransportDirect:
		return TransportDirect
	case TransportSet:
		return TransportSet
	default:
		return TransportStreamPNG
	}
}

// Options configures a Viewer. Fields are read directly from environment
// variables by cmd/herdr-pdf; keeping them as plain fields (not reading
// os.Getenv inside this package) is what makes Run() testable without a
// real herdr pane.
type Options struct {
	PDFPath   string // PDFVIEW_FILE, or argv[1]
	PaneID    string // HERDR_PANE_ID
	LogPath   string
	Transport Transport // HERDR_PDF_TRANSPORT
	Trace     bool      // HERDR_PDF_TRACE=1

	// MaxFrameKB caps the encoded page size, from HERDR_PDF_MAX_FRAME_KB.
	// Zero means "decide from the session": see resolveTransport.
	MaxFrameKB int

	// Progressive is HERDR_PDF_PROGRESSIVE. Nil means "decide from the
	// session" — on for a remote client, off for a local one.
	Progressive *bool
}

// LogPath resolves where viewer.log should live: the plugin's own state
// directory when running under herdr, /tmp otherwise (so it still produces
// a log when run standalone for debugging).
func LogPath() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return filepath.Join(d, "viewer.log")
	}
	return "/tmp/herdr-pdf-viewer.log"
}

type Viewer struct {
	opts     Options
	renderer *render.Renderer
	term     *ui.Terminal
	stream   *herdr.StreamSession
	logf     *os.File

	// layerID is resolved at startup, not fixed: TransportDirect must use
	// "primary" to reach herdr's zero-copy path (see the const block).
	layerID   string
	transport Transport

	// frameDir is pane.graphics.info's file_frame_directory, and frameSeq
	// rotates file names within it. Rotation matters: herdr keeps the
	// leased file open while it draws (pane_graphics_files.rs:64-81), so
	// overwriting the path we just sent could change the bytes underneath
	// a frame still in flight.
	frameDir string
	frameSeq uint64

	// info is cached between redraws. Re-fetching it on every keypress
	// costs a fresh Unix-socket dial and a round trip through herdr's
	// single-threaded headless event loop — the same loop that renders —
	// so on a held-down j it queues behind the very work it is waiting on.
	infoMu      sync.Mutex
	info        *herdr.GraphicsInfo
	infoAt      time.Time
	infoCols    int
	infoRows    int
	infoRefresh bool

	// lastSetBytes carries the graphics.set fallback's payload size back
	// out to the caller, which reports bytes for every transport uniformly.
	lastSetBytes int

	// lastBaseBytes is the truecolour size of the page just sent, logged so
	// the colour ladder's saving is visible per page.
	lastBaseBytes int

	page       int
	numBuf     string
	needRedraw bool
	keyAt      time.Time

	// maxFrameBytes is the budget EncodeAdaptive works to. It is much
	// tighter on a remote session, where every byte crosses an SSH link.
	maxFrameBytes int
	remote        bool
	progressive   bool

	// redrawGen invalidates a pending refinement: every redraw bumps it, so
	// a refinement scheduled for a page the reader has already flipped past
	// notices it has been superseded and sends nothing.
	redrawGen atomic.Uint64

	// lastDelta remembers which way the reader is moving so the prefetcher
	// warms the page they are about to ask for, not the one behind them.
	lastDelta int

	// pngCache holds already-encoded frames, filled by the prefetcher.
	// Guarded by pngMu because the prefetch goroutine writes it while the
	// main loop reads it.
	pngMu    sync.Mutex
	pngCache map[pngKey]encodedPage
	pngOrder []pngKey
}

// encodedPage is one page compressed both ways: the full-resolution frame
// and, when progressive refinement is on, the small stand-in sent first.
type encodedPage struct {
	full    render.Encoded
	preview render.Encoded
}

// pngKey identifies an encoded frame. Frame pixel dimensions are part of
// the key, not just the page number, so a resize cannot serve a stale
// encode at the old size.
type pngKey struct{ page, w, h int }

// pngCacheCap matches the renderer's own cache: the current page plus both
// neighbours, with room for one stale size left over from a resize.
const pngCacheCap = 4

// Frame size budgets, in bytes.
//
// A remote session reaches the terminal over SSH, which herdr neither
// compresses nor pipelines: the payload is bincode'd uncompressed into the
// same message as the pane's text surface, and the render channel to that
// client holds one frame and drops rather than queues when it is busy
// (server/client_transport.rs:119-124). 200 KB is the budget that keeps a
// page comfortably inside one such message alongside the text.
//
// Locally the bytes only cross a Unix socket, so the budget is loose
// enough that an ordinary page keeps full colour and never steps down.
const (
	remoteFrameBudget = 200 << 10
	localFrameBudget  = 1500 << 10
)

// Progressive refinement parameters.
//
// previewBudget is sized to clear a slow link inside one render tick.
// refineDelay is the pause before the full-resolution frame follows: it has
// to exceed herdr's own render cadence (MIN_RENDER_INTERVAL = 16 ms,
// app/mod.rs:38) or the server would coalesce both frames into one tick and
// the preview would never be drawn at all, making the whole exercise a pure
// cost. It is also the window in which a second keypress cancels the
// refinement outright, so holding j down sends only previews — which is
// exactly when sharpness matters least and latency matters most.
const (
	previewBudget = 32 << 10
	refineDelay   = 90 * time.Millisecond
)

// infoTTL is how old a cached pane.graphics.info may get before a refresh
// is started. Cell size is not actually stable for a pane — the same pane
// reported 8x19 and 15x38 px in ARCH-review's "邊界案例矩陣" when a different
// client attached — so it is re-read rather than trusted for the life of
// the process. Expiry only *starts* a refresh; see graphicsInfo for why it
// never makes a keypress wait for one.
const infoTTL = 2 * time.Second

func New(opts Options) *Viewer {
	return &Viewer{opts: opts}
}

func openLog(path string) *os.File {
	if path == "" {
		return nil
	}
	// Truncate once it grows past 1 MB rather than rotating — this is a
	// diagnostic log for one plugin, not an audit trail (ARCH-review P2:
	// viewer.log "無上限無輪替").
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		_ = os.Truncate(path, 0)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

func (v *Viewer) log(format string, args ...interface{}) {
	if v.logf == nil {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05.000000")
	fmt.Fprintf(v.logf, ts+" "+format+"\n", args...)
}

// traceSpan accumulates per-stage durations for one redraw and emits them
// as a single line when $HERDR_PDF_TRACE=1. It measures from keyAt — the
// moment the keypress was pulled off the input channel — rather than from
// the start of fullRedraw, because the question being answered is "how long
// after I pressed j does the page change", and any wait before the redraw
// begins counts against that just as much as the render does.
type traceSpan struct {
	v     *Viewer
	on    bool
	start time.Time
	prev  time.Time
	parts []string
}

func (v *Viewer) beginTrace() *traceSpan {
	t := &traceSpan{v: v, on: v.opts.Trace}
	if !t.on {
		return t
	}
	t.start = time.Now()
	t.prev = t.start
	if !v.keyAt.IsZero() {
		t.parts = append(t.parts, fmt.Sprintf("key_to_redraw=%.1f", msBetween(v.keyAt, t.start)))
	}
	return t
}

func (t *traceSpan) mark(name string) {
	if !t.on {
		return
	}
	now := time.Now()
	t.parts = append(t.parts, fmt.Sprintf("%s=%.1f", name, msBetween(t.prev, now)))
	t.prev = now
}

func (t *traceSpan) done(extra string) {
	if !t.on {
		return
	}
	end := time.Now()
	base := t.start
	if !t.v.keyAt.IsZero() {
		base = t.v.keyAt
	}
	t.v.log("TRACE %s %s total=%.1f key_at=%s done_at=%s",
		extra, strings.Join(t.parts, " "), msBetween(base, end),
		t.v.keyAt.Format("15:04:05.000000"), end.Format("15:04:05.000000"))
}

func msBetween(a, b time.Time) float64 {
	return float64(b.Sub(a).Microseconds()) / 1000.0
}

// Run drives the viewer until the user quits, stdin hits EOF, or an
// unrecoverable error occurs. Every return path leaves the terminal exactly
// as it found it: raw-mode/alt-screen entry and its restoration are always
// paired via defer, registered back-to-back with nothing risky in between,
// and a top-level recover keeps a panic from a rendering bug leaving the
// pane's terminal stuck in raw mode with a hidden cursor.
func (v *Viewer) Run() (err error) {
	v.logf = openLog(v.opts.LogPath)
	defer func() {
		if v.logf != nil {
			v.logf.Close()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			v.log("PANIC: %v", r)
			if err == nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}
	}()

	if v.opts.PDFPath == "" {
		return v.usageScreen("沒有指定 PDF 檔案 (PDFVIEW_FILE is empty)")
	}
	if fi, statErr := os.Stat(v.opts.PDFPath); statErr != nil || fi.IsDir() {
		return v.usageScreen(fmt.Sprintf("找不到檔案: %s", v.opts.PDFPath))
	}
	if v.opts.PaneID == "" {
		return v.usageScreen("缺少 HERDR_PANE_ID，無法呼叫 pane.graphics API")
	}

	// pdfium init happens before raw mode: a cold ~1.3s wazero compile
	// (G1-report) should show as a plain blocking process start, not a
	// raw-mode screen with a hidden cursor and nothing on it.
	r, rerr := render.NewRenderer(render.WazeroCacheDir())
	if rerr != nil {
		v.log("pdfium init failed: %v", rerr)
		return v.usageScreen("無法啟動 PDF 引擎（詳見 viewer.log）")
	}
	v.renderer = r
	defer r.Close()

	if oerr := r.Open(v.opts.PDFPath); oerr != nil {
		v.log("open %s failed: %v", v.opts.PDFPath, oerr)
		return v.usageScreen(fmt.Sprintf("無法開啟 PDF（詳見 viewer.log）: %s", filepath.Base(v.opts.PDFPath)))
	}

	v.term = ui.New()
	if terr := v.term.Enter(); terr != nil {
		return fmt.Errorf("enter raw mode: %w", terr)
	}
	defer v.term.Restore()
	defer func() {
		if cerr := herdr.GraphicsClear(v.opts.PaneID, v.layerID); cerr != nil {
			v.log("final graphics.clear: %v", cerr)
		}
	}()

	v.resolveTransport()
	if v.transport != TransportSet {
		if s, serr := herdr.OpenStream(v.opts.PaneID, v.layerID, 0); serr != nil {
			v.log("stream open failed, starting in graphics.set mode: %v", serr)
			v.transport = TransportSet
		} else {
			v.stream = s
		}
	}
	v.log("transport=%s layer=%s remote=%v progressive=%v max_frame_kb=%d frame_dir=%q trace=%v",
		v.transport, v.layerID, v.remote, v.progressive, v.maxFrameBytes>>10, v.frameDir, v.opts.Trace)
	defer func() {
		if v.stream != nil {
			v.stream.Close()
		}
	}()

	v.fullRedraw()

	reader := bufio.NewReader(os.Stdin)
	keysCh := ui.StartKeyReader(reader)

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	const resizeDebounce = 150 * time.Millisecond
	var resizeTimer *time.Timer
	var resizeFire <-chan time.Time

	for {
		select {
		case ev, ok := <-keysCh:
			if !ok {
				return nil
			}
			// Stamped here rather than inside ReadKey so the measurement
			// includes any time the event spent queued: the reader
			// goroutine blocks on the fd, so this is within microseconds of
			// the byte arriving, but a burst of held-down j keys shows up
			// as real queueing rather than being hidden.
			v.keyAt = time.Now()
			if v.handleKey(ev) {
				return nil
			}
		drain:
			for {
				select {
				case ev2, ok2 := <-keysCh:
					if !ok2 {
						return nil
					}
					if v.handleKey(ev2) {
						return nil
					}
				default:
					break drain
				}
			}
			if v.needRedraw {
				v.needRedraw = false
				v.fullRedraw()
			}

		case <-sigCh:
			// A resize is the one event that genuinely invalidates cached
			// cell geometry, so drop the cache here instead of paying for a
			// pane.graphics.info round trip on every page flip.
			v.infoMu.Lock()
			v.info = nil
			v.infoMu.Unlock()
			if resizeTimer != nil {
				resizeTimer.Stop()
			}
			resizeTimer = time.NewTimer(resizeDebounce)
			resizeFire = resizeTimer.C

		case <-resizeFire:
			resizeFire = nil
			v.keyAt = time.Time{}
			v.fullRedraw()
		}
	}
}

// handleKey applies one key event to viewer state, returning true if the
// viewer should quit.
func (v *Viewer) handleKey(ev ui.KeyEvent) bool {
	if ev.Key == ui.KeyEOF {
		return true
	}
	if ev.Key == ui.KeyRune {
		switch ev.Rune {
		case 'q', 0x03: // Ctrl-C
			return true
		case 'j', ' ', 'n':
			v.numBuf = ""
			v.nextPage()
		case 'k', 'p':
			v.numBuf = ""
			v.prevPage()
		case 'g':
			v.numBuf = ""
			v.firstPage()
		case 'G':
			v.jumpOrLast()
		case 'r':
			v.numBuf = ""
			v.needRedraw = true
		case '\r', '\n':
			v.confirmJump()
		default:
			if ev.Rune >= '0' && ev.Rune <= '9' {
				v.numBuf += string(ev.Rune)
			} else {
				v.numBuf = ""
			}
		}
		return false
	}
	switch ev.Key {
	case ui.KeyDown, ui.KeyPageDown:
		v.numBuf = ""
		v.nextPage()
	case ui.KeyUp, ui.KeyPageUp:
		v.numBuf = ""
		v.prevPage()
	}
	return false
}

func (v *Viewer) nextPage() {
	if v.page < v.renderer.PageCount()-1 {
		v.page++
		v.lastDelta = 1
		v.needRedraw = true
	}
}

func (v *Viewer) prevPage() {
	if v.page > 0 {
		v.page--
		v.lastDelta = -1
		v.needRedraw = true
	}
}

func (v *Viewer) firstPage() {
	if v.page != 0 {
		v.page = 0
		v.needRedraw = true
	}
}

func (v *Viewer) lastPage() {
	if last := v.renderer.PageCount() - 1; v.page != last {
		v.page = last
		v.needRedraw = true
	}
}

func (v *Viewer) jumpOrLast() {
	if v.numBuf != "" {
		v.confirmJump()
		return
	}
	v.lastPage()
}

// confirmJump applies a pending numeric-prefix page jump (1-based input,
// clamped into range), matching the "數字後接 G 或 Enter 跳頁" keybinding.
func (v *Viewer) confirmJump() {
	if v.numBuf == "" {
		return
	}
	n, err := strconv.Atoi(v.numBuf)
	v.numBuf = ""
	if err != nil {
		return
	}
	n-- // 1-based user input -> 0-based page index
	if n < 0 {
		n = 0
	}
	if last := v.renderer.PageCount() - 1; n > last {
		n = last
	}
	if n != v.page {
		v.page = n
		v.needRedraw = true
	}
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return 1
	}
	if a <= 0 {
		return 1
	}
	return (a + b - 1) / b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func computePlacement(cols, drawRows, cellW, cellH, frameW, frameH int) herdr.Placement {
	gridCols := ceilDiv(frameW, cellW)
	gridRows := ceilDiv(frameH, cellH)
	return herdr.Placement{
		ViewportCol: maxInt(0, (cols-gridCols)/2),
		ViewportRow: maxInt(0, (drawRows-gridRows)/2),
		GridCols:    gridCols,
		GridRows:    gridRows,
	}
}

// resolveTransport picks the frame transport for this session and, for
// TransportDirect, verifies up front that herdr will actually honour it.
//
// The check is not optional. file_frame_transport only reports
// "direct-kitty" when the *client* handshake found a terminal worth using
// it on (client/handshake.rs:57-84: ghostty/wezterm/kitty, not behind
// ssh/tmux/screen, real tty on both ends), and even then the server ANDs
// that with the layer being "primary" and the format being rgba
// (app/api/pane_graphics.rs:312-315). Miss any of those and herdr silently
// falls back to lease.copy_rgba() — a full re-read of the file we just
// wrote — which is strictly worse than having sent the bytes inline. So a
// direct transport that cannot be confirmed degrades to the PNG default
// rather than quietly paying double.
func (v *Viewer) resolveTransport() {
	v.transport = v.opts.Transport
	if v.transport == "" {
		v.transport = TransportStreamPNG
	}
	v.layerID = namedLayerID

	// file_frame_transport is negotiated by the client handshake, and it is
	// only ever "direct-kitty" for a terminal on the same machine
	// (client/handshake.rs:57-84 refuses it behind ssh/tmux/screen). An
	// empty value therefore means one of two things — a remote client, or
	// no client attached — and both of them mean inline frames over a link
	// worth spending encode time to shorten. That makes it the signal for
	// picking the frame budget as well as for refusing the direct path.
	info, err := herdr.PaneGraphicsInfo(v.opts.PaneID)
	local := err == nil && info.FileFrameTransport == "direct-kitty" && info.FileFrameDirectory != ""
	v.remote = !local

	switch {
	case v.opts.Progressive != nil:
		v.progressive = *v.opts.Progressive
	default:
		v.progressive = v.remote
	}

	switch {
	case v.opts.MaxFrameKB > 0:
		v.maxFrameBytes = v.opts.MaxFrameKB << 10
	case v.remote:
		v.maxFrameBytes = remoteFrameBudget
	default:
		v.maxFrameBytes = localFrameBudget
	}

	if v.transport != TransportDirect {
		return
	}
	if !local {
		reason := "file_frame_transport is not direct-kitty"
		if err != nil {
			reason = fmt.Sprintf("info failed: %v", err)
		}
		v.log("direct transport unavailable (%s), using %s", reason, TransportStreamPNG)
		v.transport = TransportStreamPNG
		return
	}
	v.frameDir = info.FileFrameDirectory
	v.layerID = primaryLayerID
}

// graphicsInfo returns cell geometry, serving a stale value while a
// refresh runs rather than making the keypress wait for one.
//
// The call is cheap when probed from outside the viewer (~0.4 ms), but
// measured from inside it the first one after an idle gap consistently
// costs ~106 ms, which at an ordinary reading pace lands on every single
// page flip. The cause was not pinned down — an external prober with the
// same idle gaps never reproduced it — so rather than rely on a theory
// about why it is slow, the fix removes it from the critical path: only a
// redraw with no usable cached geometry at all (startup, and the first
// redraw after a resize changes the pane's cell dimensions) blocks on it.
func (v *Viewer) graphicsInfo(cols, drawRows int) (*herdr.GraphicsInfo, error) {
	v.infoMu.Lock()
	cached, at := v.info, v.infoAt
	usable := cached != nil && v.infoCols == cols && v.infoRows == drawRows
	v.infoMu.Unlock()

	if usable {
		if time.Since(at) >= infoTTL {
			v.refreshInfoAsync(cols, drawRows)
		}
		return cached, nil
	}

	info, err := herdr.PaneGraphicsInfo(v.opts.PaneID)
	if err != nil {
		return nil, err
	}
	v.storeInfo(info, cols, drawRows)
	return info, nil
}

func (v *Viewer) storeInfo(info *herdr.GraphicsInfo, cols, drawRows int) {
	v.infoMu.Lock()
	v.info, v.infoAt, v.infoCols, v.infoRows = info, time.Now(), cols, drawRows
	v.infoMu.Unlock()
}

// refreshInfoAsync re-reads geometry on a background goroutine, at most one
// at a time. A failure is left alone deliberately: the cached value is
// still the best answer available, and a transient socket error must not
// blank the page the reader is looking at.
func (v *Viewer) refreshInfoAsync(cols, drawRows int) {
	v.infoMu.Lock()
	if v.infoRefresh {
		v.infoMu.Unlock()
		return
	}
	v.infoRefresh = true
	v.infoMu.Unlock()

	go func() {
		info, err := herdr.PaneGraphicsInfo(v.opts.PaneID)
		v.infoMu.Lock()
		v.infoRefresh = false
		if err == nil && info.CellWidthPx > 0 && info.CellHeightPx > 0 {
			v.info, v.infoAt, v.infoCols, v.infoRows = info, time.Now(), cols, drawRows
		}
		v.infoMu.Unlock()
	}()
}

// showStatus repaints the text layer. The screen clear lives here, at the
// last possible moment, rather than at the top of fullRedraw: clearing
// first blanks the pane for the whole render-and-send window, which on the
// RGBA transport was the largest visible part of a page flip.
func (v *Viewer) showStatus(cols, rows int, right string) {
	ui.ClearScreen()
	v.writeStatus(cols, rows, right)
}

// fullRedraw re-queries geometry, renders the current page to fit, pushes
// it to the pane, and repaints the status line. Every failure path still
// draws a status line explaining what went wrong instead of leaving the
// pane blank with no clue (ARCH-review P0: viewer.py's error handling only
// distinguished success/failure, never surfacing herdr's error.code, so a
// user opening a scan on an unsupported terminal just saw "render 失敗").
func (v *Viewer) fullRedraw() {
	v.redrawGen.Add(1)
	tr := v.beginTrace()

	cols, rows, err := ui.Size()
	if err != nil {
		v.log("winsize failed: %v", err)
		cols, rows = 80, 24
	}
	if rows < 2 {
		rows = 2
	}
	if cols < 1 {
		cols = 1
	}
	drawRows := rows - 1

	info, err := v.graphicsInfo(cols, drawRows)
	tr.mark("info")
	if err != nil {
		v.log("pane.graphics.info failed: %v", err)
		v.showStatus(cols, rows, v.errorStatus(err))
		return
	}
	if info.CellWidthPx <= 0 || info.CellHeightPx <= 0 {
		v.log("pane.graphics.info missing cell size: %+v", info)
		v.showStatus(cols, rows, "此終端不支援 Kitty graphics")
		return
	}

	canvasW := cols * info.CellWidthPx
	canvasH := drawRows * info.CellHeightPx

	t0 := time.Now()
	frame, err := v.renderer.RenderPage(v.page, canvasW, canvasH)
	renderMS := msSince(t0)
	tr.mark("render")
	if err != nil {
		v.log("render page=%d failed: %v", v.page+1, err)
		v.showStatus(cols, rows, "render 失敗，見 viewer.log")
		return
	}

	placement := computePlacement(cols, drawRows, info.CellWidthPx, info.CellHeightPx, frame.Width, frame.Height)

	t1 := time.Now()
	strategy, sentBytes, sendErr := v.sendFrame(frame, placement, cols, drawRows, info.CellWidthPx, info.CellHeightPx, tr)
	sendMS := msSince(t1)

	if sendErr != nil {
		v.log("page=%d/%d send failed strategy=%s err=%v", v.page+1, v.renderer.PageCount(), strategy, sendErr)
		v.showStatus(cols, rows, v.errorStatus(sendErr))
		return
	}

	v.log(
		"page=%d/%d canvas=%dx%d cell=%dx%d frame=%dx%d rgba_bytes=%d rgb_png_bytes=%d sent_bytes=%d render_ms=%.1f send_ms=%.1f strategy=%s",
		v.page+1, v.renderer.PageCount(), cols, drawRows, info.CellWidthPx, info.CellHeightPx,
		frame.Width, frame.Height, len(frame.RGBA), v.lastBaseBytes, sentBytes, renderMS, sendMS, strategy,
	)

	right := fmt.Sprintf("%s %dx%d %.0fms", strategy, frame.Width, frame.Height, renderMS+sendMS)
	v.showStatus(cols, rows, right)
	tr.mark("status")
	tr.done(fmt.Sprintf("page=%d strategy=%s sent_bytes=%d", v.page+1, strategy, sentBytes))

	v.prefetchNeighbours(canvasW, canvasH)
}

// prefetchNeighbours warms the pages either side of the current one on a
// background goroutine, after the current page is already on screen.
//
// Rendering is the largest measured cost on the keypress path — 25-70 ms
// for a fit-to-pane page, against ~0.3 ms for the geometry lookup and a few
// ms for the socket write — and for sequential reading it is entirely
// predictable work. Doing it in the gap between keypresses turns the next
// flip's RenderPage into a cache hit. The pages to warm are chosen before
// the goroutine starts, so it never reads viewer state the main loop is
// concurrently mutating; pdfium calls serialise on the renderer's own lock.
func (v *Viewer) prefetchNeighbours(canvasW, canvasH int) {
	order := []int{v.page + 1, v.page - 1}
	if v.lastDelta < 0 {
		order = []int{v.page - 1, v.page + 1}
	}
	var want []int
	for _, p := range order {
		if p >= 0 && p < v.renderer.PageCount() && !v.renderer.Cached(p, canvasW, canvasH) {
			want = append(want, p)
		}
	}
	if len(want) == 0 {
		return
	}
	r := v.renderer
	encodeToo := v.transport == TransportStreamPNG
	go func() {
		for _, p := range want {
			frame, err := r.RenderPage(p, canvasW, canvasH)
			if err != nil {
				continue
			}
			if !encodeToo {
				continue
			}
			enc, eerr := v.encodePage(frame)
			if eerr != nil {
				continue
			}
			v.storePNGAt(p, frame, enc)
		}
	}()
}

func msSince(t0 time.Time) float64 {
	return float64(time.Since(t0).Microseconds()) / 1000.0
}

func (v *Viewer) writeStatus(cols, rows int, right string) {
	name := filepath.Base(v.opts.PDFPath)
	total := "?"
	if v.renderer != nil {
		total = strconv.Itoa(v.renderer.PageCount())
	}
	mid := fmt.Sprintf("頁 %d/%s  [j/k 翻頁 g/G 首尾 q 離開]", v.page+1, total)
	line := ui.StatusLine(name, mid, right, cols)
	os.Stdout.WriteString(ui.RenderStatusLine(rows, line))
}

// errorStatus turns a herdr *Error (or any other error) into the specific,
// actionable status-line message ARCH-review's P0 finding said viewer.py
// was missing: it parsed only success/failure, never resp.error.code, so a
// terminal without Kitty graphics support and a disabled feature flag both
// rendered as the same opaque "render 失敗".
func (v *Viewer) errorStatus(err error) string {
	if he, ok := err.(*herdr.Error); ok {
		switch he.Code {
		case "cell_size_unavailable":
			return "此終端不支援 Kitty graphics"
		case "feature_disabled":
			return "請在 config.toml 開啟 [terminal] kitty_graphics"
		case "pane_not_found":
			return "pane 不存在，viewer 即將失去畫面"
		case "image_too_large":
			return "此頁太大，見 viewer.log"
		default:
			return "錯誤: " + he.Code
		}
	}
	return "render 失敗，見 viewer.log"
}

// sendFrame pushes one rendered page using the configured transport,
// falling back to the short-lived graphics.set PNG path whenever the
// long-lived stream is unavailable or has failed.
//
// The transport choice is the whole performance story. All four end up as
// Kitty graphics in the outer terminal, but they differ by more than an
// order of magnitude in how many bytes get there: herdr applies no
// compression of its own anywhere on this path — no zlib, no o=z — so
// whatever the plugin hands over is what gets hashed, copied into the
// inline layer, bincode-encoded onto the client wire uncompressed, then
// base64-expanded 4:3 and cut into 3072-byte Kitty chunks.
func (v *Viewer) sendFrame(frame *render.Frame, placement herdr.Placement, cols, drawRows, cellW, cellH int, tr *traceSpan) (strategy string, sentBytes int, err error) {
	if v.stream != nil {
		if failErr := v.stream.Failed(); failErr != nil {
			v.log("stream had failed, switching to graphics.set: %v", failErr)
			v.stream.Close()
			v.stream = nil
		}
	}

	if v.stream != nil {
		strategy, sentBytes, err = v.sendViaStream(frame, placement, tr)
		if err == nil {
			return strategy, sentBytes, nil
		}
		v.log("stream send failed (%s), falling back to graphics.set: %v", strategy, err)
		v.stream.Close()
		v.stream = nil
	}

	strategy, err = v.sendViaSet(frame, cols, drawRows, cellW, cellH)
	return strategy, v.lastSetBytes, err
}

// sendViaStream dispatches one frame on the open stream session according
// to v.transport.
func (v *Viewer) sendViaStream(frame *render.Frame, placement herdr.Placement, tr *traceSpan) (strategy string, sentBytes int, err error) {
	switch v.transport {
	case TransportStreamRGBA:
		if serr := v.stream.SendFrame(frame.RGBA, frame.Width, frame.Height, placement); serr != nil {
			return "stream-rgba", len(frame.RGBA), serr
		}
		return "stream-rgba", len(frame.RGBA), nil

	case TransportDirect:
		path, werr := v.writeFrameFile(frame)
		tr.mark("file_write")
		if werr != nil {
			return "direct", 0, werr
		}
		// Unlike an inline frame, this one has a real acknowledgement:
		// herdr answers every file frame, success included
		// (pane_graphics_stream.rs:253-254), so the time spent here is the
		// server actually taking the frame rather than a socket write
		// disappearing into a buffer.
		if serr := v.stream.SendFile("rgba", path, frame.Width, frame.Height, placement); serr != nil {
			return "direct", len(frame.RGBA), serr
		}
		return "direct", len(frame.RGBA), nil

	default: // TransportStreamPNG
		// A prefetched encode is the common case while reading in one
		// direction, which is what keeps the colour-depth ladder — up to
		// three encodes of a 2.8 megapixel page — off the keypress path.
		page, hit := v.lookupPNG(frame)
		if !hit {
			var eerr error
			page, eerr = v.encodePage(frame)
			if eerr != nil {
				tr.mark("png_encode")
				return "stream-png", 0, eerr
			}
			v.storePNGAt(v.page, frame, page)
		}
		if hit {
			tr.mark("png_cached")
		} else {
			tr.mark("png_encode")
		}
		v.lastBaseBytes = page.full.BaseBytes

		first := page.full
		if v.progressive && page.preview.Payload != nil {
			first = page.preview
		}
		if serr := v.sendEncoded(v.stream, first, placement); serr != nil {
			return "stream-png", len(first.Payload), serr
		}
		if first.Strategy == page.full.Strategy {
			return fmt.Sprintf("png:%s(%dKB)", page.full.Strategy, len(page.full.Payload)/1024), len(page.full.Payload), nil
		}

		v.scheduleRefine(page.full, placement)
		return fmt.Sprintf("png:%s(%dKB)->%s(%dKB)",
			first.Strategy, len(first.Payload)/1024,
			page.full.Strategy, len(page.full.Payload)/1024), len(first.Payload), nil
	}
}

// encodePage runs the colour ladder, plus the preview encode when
// progressive refinement is on.
func (v *Viewer) encodePage(frame *render.Frame) (encodedPage, error) {
	full, err := render.EncodeAdaptive(frame, v.maxFrameBytes)
	if err != nil {
		return encodedPage{}, err
	}
	out := encodedPage{full: full}
	// No point previewing a page that is already small: the stand-in would
	// cost an extra frame on the link to save almost nothing.
	if v.progressive && len(full.Payload) > 2*previewBudget {
		if pv, perr := render.EncodePreview(frame, previewBudget); perr == nil && len(pv.Payload) < len(full.Payload) {
			out.preview = pv
		}
	}
	return out, nil
}

// sendEncoded pushes one encoded frame, always at the placement computed
// for the page at full size.
//
// This is what makes a scaled-down encode usable. herdr writes the image's
// own pixel size into the Kitty control as s= and v=, but takes c= and r=
// — the cell box the image is drawn into — from the placement
// (kitty_graphics.rs:799, :820). The terminal scales the image to fill that
// box. So a half-size preview or a shrunk rung sent with the full page's
// placement fills the same area, just softer, instead of appearing as a
// smaller picture that snaps to size when the sharp frame replaces it.
func (v *Viewer) sendEncoded(st *herdr.StreamSession, enc render.Encoded, placement herdr.Placement) error {
	return st.SendInline("png", enc.Payload, enc.Width, enc.Height, placement)
}

// scheduleRefine sends the full-resolution frame a short time after the
// preview, unless the reader has moved on in the meantime.
//
// The stream pointer is captured rather than re-read: if the main loop
// tears the session down and falls back to graphics.set while this is in
// flight, the send fails on a closed connection and is logged, instead of
// racing the field the main loop is reassigning.
func (v *Viewer) scheduleRefine(full render.Encoded, placement herdr.Placement) {
	gen := v.redrawGen.Load()
	st := v.stream
	go func() {
		time.Sleep(refineDelay)
		if v.redrawGen.Load() != gen {
			return
		}
		if err := v.sendEncoded(st, full, placement); err != nil {
			v.log("refine send failed: %v", err)
			return
		}
		v.log("refine sent strategy=%s bytes=%d", full.Strategy, len(full.Payload))
	}()
}

func (v *Viewer) lookupPNG(frame *render.Frame) (encodedPage, bool) {
	v.pngMu.Lock()
	defer v.pngMu.Unlock()
	e, ok := v.pngCache[pngKey{v.page, frame.Width, frame.Height}]
	return e, ok
}

func (v *Viewer) storePNGAt(page int, frame *render.Frame, enc encodedPage) {
	v.pngMu.Lock()
	defer v.pngMu.Unlock()
	if v.pngCache == nil {
		v.pngCache = make(map[pngKey]encodedPage)
	}
	k := pngKey{page, frame.Width, frame.Height}
	if _, exists := v.pngCache[k]; exists {
		return
	}
	v.pngCache[k] = enc
	v.pngOrder = append(v.pngOrder, k)
	for len(v.pngOrder) > pngCacheCap {
		delete(v.pngCache, v.pngOrder[0])
		v.pngOrder = v.pngOrder[1:]
	}
}

// writeFrameFile writes the RGBA buffer into pane.graphics.info's
// file_frame_directory and returns its absolute path.
//
// The file name rotates over a small ring rather than being reused,
// because herdr holds the leased file open and re-validates its size and
// identity while drawing (pane_graphics_files.rs:64-81, 118-131).
// Overwriting the path that a frame still in flight refers to would trip
// "frame length changed while leased" at best and swap pixels underneath a
// live draw at worst.
func (v *Viewer) writeFrameFile(frame *render.Frame) (string, error) {
	if v.frameDir == "" {
		return "", fmt.Errorf("no file_frame_directory for direct transport")
	}
	v.frameSeq++
	name := fmt.Sprintf("herdr-pdf-%d.rgba", v.frameSeq%4)
	path := filepath.Join(v.frameDir, name)
	// Truncate-and-write rather than write-then-rename: herdr validates
	// that the path is a direct child of the source directory and refuses
	// to follow symlinks, and a rename would leave the old inode leased.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create frame file: %w", err)
	}
	if _, werr := f.Write(frame.RGBA); werr != nil {
		f.Close()
		return "", fmt.Errorf("write frame file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return "", fmt.Errorf("close frame file: %w", cerr)
	}
	return path, nil
}

// sendViaSet is the PNG-over-graphics.set fallback. It fixes two P0 bugs
// ARCH-review found in viewer.py's equivalent loop: the shrink loop here
// has a real termination condition (it stops re-encoding once a candidate
// fits, and reports failure instead of silently sending an oversized PNG
// after exhausting its attempts), and a rejection's error.code is inspected
// so a genuine image_too_large retries the shrink instead of being treated
// as a generic failure.
func (v *Viewer) sendViaSet(frame *render.Frame, cols, drawRows, cellW, cellH int) (strategy string, err error) {
	cur := frame
	scale := 1.0

	for attempt := 0; attempt < setMaxTries; attempt++ {
		enc, encErr := render.EncodeAdaptive(cur, setMaxBytes)
		if encErr != nil {
			return "set", encErr
		}
		if len(enc.Payload) <= setMaxBytes {
			placement := computePlacement(cols, drawRows, cellW, cellH, enc.Width, enc.Height)
			setErr := herdr.GraphicsSet(v.opts.PaneID, enc.Payload, enc.Width, enc.Height, v.layerID, placement)
			if setErr == nil {
				v.lastSetBytes = len(enc.Payload)
				return fmt.Sprintf("set:%s(%dKB)", enc.Strategy, len(enc.Payload)/1024), nil
			}
			if he, ok := setErr.(*herdr.Error); !ok || he.Code != "image_too_large" {
				return "set", setErr
			}
			// image_too_large despite our own size check passing: the
			// server compares decoded bytes, which can differ slightly
			// from our PNG byte count. Fall through and shrink further.
		}

		var w, h int
		var ok bool
		scale, w, h, ok = planShrink(attempt, frame.Width, frame.Height, scale)
		if !ok {
			break
		}
		next, rerr := v.renderer.RenderPage(v.page, w, h)
		if rerr != nil {
			return "set", rerr
		}
		cur = next
	}

	// Every P0 fix here in one place: unlike viewer.py's equivalent loop,
	// which unconditionally returned the last (still oversized) PNG after
	// exhausting its attempts, this reports failure and sends nothing.
	return "set", fmt.Errorf("此頁太大：縮放 %d 次後仍超過 %dKB 上限，未送出", setMaxTries, setMaxBytes/1024)
}

// planShrink computes the next (scale, width, height) to retry rendering
// at, or ok=false once attempt has reached setMaxTries. Kept pure (no I/O)
// so the termination condition — the exact thing viewer.py got wrong — is
// unit-testable without a live pdfium renderer or herdr socket.
func planShrink(attempt, baseW, baseH int, scale float64) (newScale float64, w, h int, ok bool) {
	if attempt >= setMaxTries-1 {
		return scale, 0, 0, false
	}
	newScale = scale * setShrinkPct
	w = maxInt(1, int(float64(baseW)*newScale))
	h = maxInt(1, int(float64(baseH)*newScale))
	return newScale, w, h, true
}

// usageScreen shows a plain-text message and waits for q/Ctrl-C/EOF. Used
// for every "can't even start" case (missing file, missing HERDR_PANE_ID,
// pdfium init failure, a PDF that won't open) so the pane explains itself
// instead of a bare shell error or a silently closed pane.
func (v *Viewer) usageScreen(reason string) error {
	t := ui.New()
	if err := t.Enter(); err != nil {
		fmt.Fprintln(os.Stderr, "herdr-pdf:", reason)
		return fmt.Errorf("%s", reason)
	}
	defer t.Restore()

	ui.ClearScreen()
	msg := "herdr PDF Viewer\r\n\r\n" + reason + "\r\n\r\n" +
		"用法: 設定環境變數 PDFVIEW_FILE=/path/to/file.pdf 後開啟本 pane，\r\n" +
		"或用 `herdr-pdf open /path/to/file.pdf`。\r\n\r\n按 q 離開。\r\n"
	os.Stdout.WriteString("\x1b[1;1H" + msg)

	reader := bufio.NewReader(os.Stdin)
	for ev := range ui.StartKeyReader(reader) {
		if ev.Key == ui.KeyEOF {
			return nil
		}
		if ev.Key == ui.KeyRune && (ev.Rune == 'q' || ev.Rune == 0x03) {
			return nil
		}
	}
	return nil
}
