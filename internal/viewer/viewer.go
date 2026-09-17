// Package viewer implements the herdr-pdf view pane's state machine.
package viewer

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tim80411/herdr-pdf/internal/browser"
	"github.com/tim80411/herdr-pdf/internal/herdr"
	"github.com/tim80411/herdr-pdf/internal/render"
	"github.com/tim80411/herdr-pdf/internal/ui"
)

// primaryLayerID is required by TransportDirect: herdr's zero-copy file
// transport only fires when the layer is literally "primary" (see herdr src
// app/api/pane_graphics.rs:312).
const (
	namedLayerID   = "page"
	primaryLayerID = "primary"
)

// Raster caps bound one page's pixel buffer. A terminal cell grid cannot
// resolve more detail than this, while RGBA memory keeps growing with every
// extra pixel: 2400x3200 is already ~31 MB per frame, held for several
// frames by the render cache.
const (
	maxRasterW = 2400
	maxRasterH = 3200
)

const (
	setMaxBytes  = 500 * 1024 // stay under the 512 KiB hard cap with headroom for base64/JSON overhead
	setMaxTries  = 6
	setShrinkPct = 0.85
)

// Transport selects how a rendered page reaches the terminal, overridable
// via $HERDR_PDF_TRANSPORT. TransportStreamPNG is the default.
type Transport string

const (
	// TransportStreamPNG sends PNG bytes over the long-lived stream (default).
	// herdr stores compressed bytes as-is with no re-encode, so a smaller
	// payload here shrinks every downstream stage (hash, wire, base64, chunks).
	TransportStreamPNG Transport = "stream-png"

	// TransportStreamRGBA sends raw, uncompressed RGBA: no encode cost, but a
	// fit-to-pane frame is ~11 MB (~14.7 MB after base64) sent on every flip.
	TransportStreamRGBA Transport = "stream-rgba"

	// TransportDirect writes RGBA to a file in pane.graphics.info's
	// file_frame_directory and sends only the path (no base64). Requires
	// file_frame_transport == "direct-kitty"; falls back to PNG otherwise.
	TransportDirect Transport = "direct"

	// TransportSet issues a short-lived pane.graphics.set call per page
	// (PNG, capped at 512 KiB decoded).
	TransportSet Transport = "set"
)

// ParseTransport maps $HERDR_PDF_TRANSPORT onto a Transport, falling back to
// the default (rather than failing) on an empty or unrecognised value.
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

// Options configures a Viewer. Fields are set from environment variables by
// cmd/herdr-pdf, keeping this package testable without a real herdr pane.
type Options struct {
	PDFPath string // PDFVIEW_FILE, or argv[1]
	// StartDir is PDFVIEW_DIR: where browser mode opens when no file was
	// named. Empty means this process's working directory.
	StartDir  string
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

// LogPath resolves where viewer.log lives: the plugin's state directory
// under herdr, or /tmp when run standalone.
func LogPath() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return filepath.Join(d, "viewer.log")
	}
	return "/tmp/herdr-pdf-viewer.log"
}

// frameSink is the open stream a frame goes out on, taken as an interface so
// the send path can be exercised without a herdr socket.
type frameSink interface {
	SendInline(format string, payload []byte, width, height int, placement herdr.Placement) error
	SendFrame(pix []byte, width, height int, placement herdr.Placement) error
	SendFile(format, path string, width, height int, placement herdr.Placement) error
	Failed() error
	Close() error
}

type Viewer struct {
	opts     Options
	renderer *render.Renderer
	term     ui.Screen
	stream   frameSink
	logf     *os.File

	// browser is non-nil while the pane is listing files. Browsing and
	// reading are two modes of one pane, sharing this terminal, this key
	// loop and this process — choosing a file does not open a second pane.
	browser *browser.Model

	// rendererOnce guards pdfium/wazero start-up, which browser mode kicks
	// off in the background so the cold compile overlaps with the user
	// choosing a file instead of landing entirely after Enter.
	rendererOnce sync.Once
	rendererErr  error

	// graphicsReady marks the transport as resolved and the stream as
	// opened; browser mode never needs either.
	graphicsReady bool

	// helping is reading mode's "?" overlay. The page image is handed back
	// while it is up, since herdr composites graphics above text.
	helping bool

	// docGen counts documents opened in this process. It tags every png
	// cache key, so an encode belonging to the file just closed can neither
	// be served for the new one nor be stored over it by a late prefetch.
	docGen uint64

	// layerID is resolved at startup (TransportDirect forces "primary").
	layerID   string
	transport Transport

	// frameDir/frameSeq: RGBA frame file directory and its rotation counter
	// (see writeFrameFile for why the name rotates).
	frameDir string
	frameSeq uint64

	// info is cached between redraws: re-fetching on every keypress would
	// round-trip herdr's single-threaded event loop, the same loop that renders.
	infoMu      sync.Mutex
	info        *herdr.GraphicsInfo
	infoAt      time.Time
	infoCols    int
	infoRows    int
	infoRefresh bool

	// lastSetBytes carries the graphics.set payload size back for uniform logging.
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

	// redrawGen invalidates a pending refinement once the reader flips past
	// the page it was scheduled for.
	redrawGen atomic.Uint64

	// lastDelta remembers which way the reader is moving so the prefetcher
	// warms the page they are about to ask for, not the one behind them.
	lastDelta int

	// pngCache holds already-encoded frames, filled by the prefetcher.
	// Guarded by pngMu: the prefetch goroutine writes it, the main loop reads it.
	// pngPending marks frames an encode has been started for but not finished.
	pngMu      sync.Mutex
	pngCache   map[pngKey]encodedPage
	pngOrder   []pngKey
	pngPending map[pngKey]bool

	// workMu guards the quit handshake: every background send registers on
	// work under it, so a goroutine either starts before quitting is set and
	// is waited for, or sees the flag and never starts.
	workMu        sync.Mutex
	work          sync.WaitGroup
	quitting      bool
	refineTimer   *time.Timer
	pendingRefine *pendingRefine
}

// pendingRefine is the single queued full-resolution follow-up frame.
type pendingRefine struct {
	gen       uint64
	full      render.Encoded
	placement herdr.Placement
	stream    frameSink
}

// encodedPage is one page compressed both ways: the full-resolution frame
// and, when progressive refinement is on, the small stand-in sent first.
type encodedPage struct {
	full    render.Encoded
	preview render.Encoded
}

// pngKey identifies an encoded frame by document, page and pixel size: a
// resize must not serve a stale encode at the old dimensions, and switching
// files must not serve the previous document's pages.
type pngKey struct {
	doc        uint64
	page, w, h int
}

// pngCacheCap matches the renderer's own cache: the current page plus both
// neighbours, with room for one stale size left over from a resize.
const pngCacheCap = 4

// Frame size budgets, in bytes. herdr never compresses anything on this
// path, and the server->client channel holds only one frame and drops it
// when busy (see herdr src server/client_transport.rs:119) — so byte count
// directly drives latency. Remote (SSH) sessions get a tight budget; local
// Unix-socket sessions don't need to.
const (
	remoteFrameBudget = 200 << 10
	localFrameBudget  = 1500 << 10
)

// refineDelay must exceed herdr's ~16ms render cadence (MIN_RENDER_INTERVAL,
// see herdr src app/mod.rs:38), or the server coalesces both frames into one
// tick and the preview is never drawn. It also doubles as the cancel window:
// a second keypress within refineDelay skips the refine entirely.
const (
	previewBudget = 32 << 10
	refineDelay   = 90 * time.Millisecond
)

// infoTTL bounds how long a cached pane.graphics.info is trusted: cell size
// is not stable across clients (a pane's reported pixel size can change
// when a different client attaches), so it's re-read periodically. Expiry
// only *starts* a refresh; see graphicsInfo for why a keypress never waits on one.
const infoTTL = 2 * time.Second

func New(opts Options) *Viewer {
	return &Viewer{opts: opts}
}

func openLog(path string) *os.File {
	if path == "" {
		return nil
	}
	// Truncate once it grows past 1 MB rather than rotating: this is a
	// diagnostic log for one plugin, not an audit trail.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 1<<20 {
		_ = os.Truncate(path, 0)
	}
	// The log records the paths of documents the reader opens, so it stays
	// owner-only even if an earlier run left it wider.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	if fi, serr := f.Stat(); serr == nil && fi.Mode().Perm() != 0o600 {
		_ = f.Chmod(0o600)
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

// traceSpan accumulates per-stage durations for one redraw, measured from
// keyAt (not fullRedraw's start) so queueing delay counts toward the total.
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

// Run drives the viewer until the user quits, stdin hits EOF, or an error
// occurs. Raw-mode entry/restore is always deferred, and a top-level
// recover prevents a panic from leaving the terminal stuck in raw mode.
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

	// A pane opened without a file browses for one instead of complaining:
	// that is what the "open" action does when nothing named a path, which
	// is the common case for a keybinding press.
	browsing := v.opts.PDFPath == ""
	if !browsing {
		if fi, statErr := os.Stat(v.opts.PDFPath); statErr != nil || fi.IsDir() {
			return v.usageScreen(fmt.Sprintf("找不到檔案: %s", v.opts.PDFPath))
		}
		// Graphics are a reading-mode concern, so a missing pane id only
		// stops the half that needs one.
		if v.opts.PaneID == "" {
			return v.usageScreen("缺少 HERDR_PANE_ID，無法呼叫 pane.graphics API")
		}
		// pdfium init happens before raw mode, so a slow cold wazero compile
		// shows as a plain blocking start rather than a raw-mode screen with a hidden cursor.
		if rerr := v.ensureRenderer(); rerr != nil {
			return v.usageScreen("無法啟動 PDF 引擎（詳見 viewer.log）")
		}
		if oerr := v.renderer.Open(v.opts.PDFPath); oerr != nil {
			v.log("open %s failed: %v", v.opts.PDFPath, oerr)
			return v.usageScreen(openFailureMessage(oerr, v.opts.PDFPath))
		}
		v.docGen++
	}
	defer v.closeRenderer()

	v.term = ui.New()
	if terr := v.term.Enter(); terr != nil {
		return fmt.Errorf("enter raw mode: %w", terr)
	}
	defer v.shutdown()

	if browsing {
		v.enterBrowsing(v.startDir(), "")
	} else if gerr := v.startGraphics(); gerr != nil {
		return gerr
	}

	v.fullRedraw()

	reader := bufio.NewReader(os.Stdin)
	keysCh := ui.StartKeyReader(reader)

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	quitCh := make(chan os.Signal, 4)
	signal.Notify(quitCh, quitSignals...)
	defer signal.Stop(quitCh)

	const resizeDebounce = 150 * time.Millisecond
	var resizeTimer *time.Timer
	var resizeFire <-chan time.Time

	for {
		select {
		case ev, ok := <-keysCh:
			if !ok {
				return nil
			}
			// Stamped here (not inside ReadKey) so held-down keys show real
			// queueing delay rather than hiding it.
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
			// Resize invalidates cached cell geometry.
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

		case sig := <-quitCh:
			v.log("received signal %v, shutting down", sig)
			return v.signalExit(sig)
		}
	}
}

// startDir is where browsing begins: PDFVIEW_DIR if it was set, else this
// process's own working directory.
func (v *Viewer) startDir() string {
	if v.opts.StartDir != "" {
		return v.opts.StartDir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// ensureRenderer starts pdfium once, whoever asks first. Browser mode calls
// it from a background goroutine to warm the wazero compile; the main loop
// calls it again before using v.renderer and blocks on that warm-up rather
// than racing it.
func (v *Viewer) ensureRenderer() error {
	v.rendererOnce.Do(func() {
		r, err := render.NewRenderer(render.WazeroCacheDir())
		if err != nil {
			v.log("pdfium init failed: %v", err)
			v.rendererErr = err
			return
		}
		v.renderer = r
	})
	return v.rendererErr
}

// closeRenderer releases pdfium. Deferred before shutdown so it runs after
// it, i.e. after every background render has been waited for.
func (v *Viewer) closeRenderer() {
	if v.renderer != nil {
		v.renderer.Close()
		v.renderer = nil
	}
}

// startGraphics resolves the transport and opens the frame stream. It runs
// on the way into reading mode rather than at start-up, because browsing
// never sends a frame.
func (v *Viewer) startGraphics() error {
	if v.graphicsReady {
		return nil
	}
	if v.opts.PaneID == "" {
		return fmt.Errorf("缺少 HERDR_PANE_ID，無法呼叫 pane.graphics API")
	}
	v.resolveTransport()
	if v.transport != TransportSet {
		if s, serr := herdr.OpenStream(v.opts.PaneID, v.layerID, 0); serr != nil {
			v.log("stream open failed, starting in graphics.set mode: %v", serr)
			v.transport = TransportSet
		} else {
			v.stream = s
		}
	}
	v.graphicsReady = true
	v.log("transport=%s layer=%s remote=%v progressive=%v max_frame_kb=%d frame_dir=%q trace=%v",
		v.transport, v.layerID, v.remote, v.progressive, v.maxFrameBytes>>10, v.frameDir, v.opts.Trace)
	return nil
}

// releaseGraphics hands the pane's graphics layer back before text is drawn
// over it: a queued refine is dropped, the stream closed (herdr refuses
// graphics.clear while one is open) and the layer cleared. startGraphics
// reopens the stream on the way back. An empty layerID means graphics were
// never started, so there is nothing to clear and herdr would reject the
// call.
func (v *Viewer) releaseGraphics(why string) {
	v.cancelRefine()
	if v.stream != nil {
		v.stream.Close()
		v.stream = nil
	}
	v.graphicsReady = false
	if v.opts.PaneID != "" && v.layerID != "" {
		if cerr := herdr.GraphicsClear(v.opts.PaneID, v.layerID); cerr != nil {
			v.log("graphics.clear %s: %v", why, cerr)
		}
	}
}

// openHelp puts reading mode's key table over the page. The page image goes
// first: herdr composites the graphics layer above the text, so an overlay
// drawn under it would be invisible.
func (v *Viewer) openHelp() {
	v.releaseGraphics("showing help")
	v.helping = true
	v.term.ClearScreen()
}

// dismissHelp closes the overlay and puts the page back, reopening the
// stream and re-sending the current frame — cheap, because the encode for
// this page and size is still in the png cache.
func (v *Viewer) dismissHelp(ev ui.KeyEvent) bool {
	if ev.Key == ui.KeyEOF {
		return true
	}
	v.helping = false
	if gerr := v.startGraphics(); gerr != nil {
		v.log("reopening graphics after help: %v", gerr)
	}
	v.term.ClearScreen()
	v.needRedraw = true
	return false
}

// drawHelp paints reading mode's key table. No graphics call is involved.
func (v *Viewer) drawHelp() {
	cols, rows, err := ui.Size()
	if err != nil {
		v.log("winsize failed: %v", err)
		cols, rows = 80, 24
	}
	v.term.WriteString(readingHelp.RenderHelp(cols, rows))
}

// enterBrowsing switches the pane to the file listing. Any page already on
// screen is cleared first: herdr composites the graphics layer above the
// text, so a leftover image would sit on top of the listing.
func (v *Viewer) enterBrowsing(dir, focus string) {
	v.releaseGraphics("entering browser")
	v.browser = browser.New(dir)
	if focus != "" {
		v.browser.Focus(focus)
	}
	v.log("browser mode at %s", v.browser.Dir())
	// Warm pdfium while the user reads the listing, so opening a file is not
	// the first thing to pay for the cold compile.
	v.startWork(func() { _ = v.ensureRenderer() })
	v.term.ClearScreen()
}

// enterReading loads path and leaves browsing mode. A failure keeps the
// listing up with the reason on its status line, since the alternative is a
// pane that has lost both the document and the way back.
func (v *Viewer) enterReading(path string) {
	if rerr := v.ensureRenderer(); rerr != nil {
		v.browser.SetMessage("無法啟動 PDF 引擎（詳見 viewer.log）")
		return
	}
	if oerr := v.renderer.Open(path); oerr != nil {
		v.log("open %s failed: %v", path, oerr)
		v.browser.SetMessage(openFailureMessage(oerr, path))
		return
	}
	if gerr := v.startGraphics(); gerr != nil {
		v.browser.SetMessage(gerr.Error())
		return
	}

	v.opts.PDFPath = path
	v.docGen++
	v.page = 0
	v.numBuf = ""
	v.lastDelta = 0
	v.browser = nil
	v.term.ClearScreen()
	v.log("reading %s (doc gen %d)", path, v.docGen)
}

// toBrowser returns from reading to the listing, parked on the file just
// read so the way back is one keypress.
func (v *Viewer) toBrowser() {
	v.enterBrowsing(filepath.Dir(v.opts.PDFPath), filepath.Base(v.opts.PDFPath))
}

// handleBrowserKey applies one key to the listing, returning true to quit.
func (v *Viewer) handleBrowserKey(ev ui.KeyEvent) bool {
	switch v.browser.HandleKey(ev) {
	case browser.ActionQuit:
		return true
	case browser.ActionRedraw:
		v.needRedraw = true
	case browser.ActionOpen:
		v.enterReading(v.browser.Chosen())
		v.needRedraw = true
	}
	return false
}

// drawBrowser paints the listing. No graphics call is involved: browsing is
// plain text on the pane's own terminal.
func (v *Viewer) drawBrowser() {
	cols, rows, err := ui.Size()
	if err != nil {
		v.log("winsize failed: %v", err)
		cols, rows = 80, 24
	}
	v.term.WriteString(v.browser.Render(cols, rows))
}

// readingHelp is the single source of truth for reading mode's keys: the
// status line's hints and the "?" overlay are both generated from it, so a
// binding cannot be advertised in one place and forgotten in the other.
// Only three bindings carry a Hint: the status row shares its width with
// the filename and the render diagnostics, and hinting everything left the
// filename as a single ellipsis. The overlay is where the rest belongs.
var readingHelp = ui.HelpTable{
	Title: "閱讀模式鍵位",
	Bindings: []ui.Binding{
		{Keys: "j  ↓  Space  n  PgDn", What: "下一頁", Hint: "j/k 翻頁"},
		{Keys: "k  ↑  p  PgUp", What: "上一頁"},
		{Keys: "g", What: "第一頁"},
		{Keys: "G", What: "跳到已輸入的頁碼，沒輸入就跳到最後一頁"},
		{Keys: "0-9 然後 G 或 Enter", What: "跳到第 N 頁（超出範圍夾到頭尾）"},
		{Keys: "r", What: "強制重繪（畫面有殘影時用）"},
		{Keys: "o", What: "回到瀏覽模式（目錄為本檔所在目錄）"},
		{Keys: "?", What: "顯示這張表", Hint: "? 說明"},
		{Keys: "q  Ctrl-c", What: "離開", Hint: "q 離開"},
	},
}

// quitSignals end the session through the same teardown as the q key.
var quitSignals = []os.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT}

// ExitSignal reports that Run stopped because of a termination signal.
type ExitSignal struct{ Sig syscall.Signal }

func (e *ExitSignal) Error() string { return "terminated by signal " + e.Sig.String() }

// ExitCode is the status a shell expects after this signal.
func (e *ExitSignal) ExitCode() int { return 128 + int(e.Sig) }

// signalExit tears the pane down and reports the status to exit with.
func (v *Viewer) signalExit(sig os.Signal) error {
	v.shutdown()
	if s, ok := sig.(syscall.Signal); ok {
		return &ExitSignal{Sig: s}
	}
	return nil
}

// shutdown is the one quit path. It cancels any queued refinement, lets
// in-flight background sends finish, and only then closes the stream, clears
// the layer, and restores the terminal — so nothing writes to a connection
// that is already gone. Calling it twice is a no-op.
func (v *Viewer) shutdown() {
	v.workMu.Lock()
	if v.quitting {
		v.workMu.Unlock()
		return
	}
	v.quitting = true
	v.redrawGen.Add(1)
	if v.refineTimer != nil {
		v.refineTimer.Stop()
	}
	v.pendingRefine = nil
	v.workMu.Unlock()

	v.work.Wait()

	if v.stream != nil {
		v.stream.Close()
		v.stream = nil
	}
	// An empty layerID means graphics were never started — a pane that only
	// ever browsed. There is nothing on the layer to clear, and herdr
	// rejects the call outright (invalid_layer_id).
	if v.opts.PaneID != "" && v.layerID != "" {
		if cerr := herdr.GraphicsClear(v.opts.PaneID, v.layerID); cerr != nil {
			v.log("final graphics.clear: %v", cerr)
		}
	}
	if v.term != nil {
		v.term.Restore()
	}
}

// cancelRefine drops any queued full-resolution follow-up frame. Bumping
// redrawGen is what makes an already-running sendRefine bail out, so the
// frame cannot land on a stream that is about to close.
func (v *Viewer) cancelRefine() {
	v.workMu.Lock()
	v.redrawGen.Add(1)
	if v.refineTimer != nil {
		v.refineTimer.Stop()
	}
	v.pendingRefine = nil
	v.workMu.Unlock()
}

// startWork runs fn on a goroutine the shutdown path waits for, and declines
// to start one at all once the viewer is quitting.
func (v *Viewer) startWork(fn func()) {
	v.workMu.Lock()
	if v.quitting {
		v.workMu.Unlock()
		return
	}
	v.work.Add(1)
	v.workMu.Unlock()
	go func() {
		defer v.work.Done()
		fn()
	}()
}

// openFailureMessage explains an Open failure in the pane, so an encrypted
// file does not read the same as a truncated one.
func openFailureMessage(err error, path string) string {
	name := filepath.Base(path)
	switch {
	case errors.Is(err, render.ErrEncrypted):
		return fmt.Sprintf("此 PDF 需要密碼，尚不支援: %s", name)
	case errors.Is(err, render.ErrCorrupt):
		return fmt.Sprintf("檔案損毀或不是 PDF: %s", name)
	}
	return fmt.Sprintf("無法開啟 PDF（詳見 viewer.log）: %s", name)
}

// handleKey applies one key event to viewer state, returning true if the
// viewer should quit.
func (v *Viewer) handleKey(ev ui.KeyEvent) bool {
	if v.browser != nil {
		return v.handleBrowserKey(ev)
	}
	if v.helping {
		// Any key returns to the page, q and Escape included: the key is
		// aimed at the overlay, not at the session.
		return v.dismissHelp(ev)
	}
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
		case 'o':
			// Back to the listing, on this file's own directory.
			v.numBuf = ""
			v.toBrowser()
			v.needRedraw = true
		case '?':
			v.numBuf = ""
			v.openHelp()
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
// clamped into range).
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

// clampRaster shrinks a canvas to the raster cap, keeping its aspect ratio.
func clampRaster(w, h int) (int, int) {
	if w <= maxRasterW && h <= maxRasterH {
		return w, h
	}
	return fitBox(w, h, maxRasterW, maxRasterH)
}

// fitBox scales w x h to the largest size that fits inside maxW x maxH.
func fitBox(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return maxW, maxH
	}
	s := math.Min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	return maxInt(1, int(float64(w)*s)), maxInt(1, int(float64(h)*s))
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
// TransportDirect, confirms up front that herdr will actually honour it —
// otherwise herdr silently falls back to a full re-read of the file we just
// wrote, which is worse than sending the bytes inline.
func (v *Viewer) resolveTransport() {
	v.transport = v.opts.Transport
	if v.transport == "" {
		v.transport = TransportStreamPNG
	}
	v.layerID = namedLayerID

	// An empty file_frame_transport means a remote client or no client at
	// all — either way, inline frames over a link worth spending encode
	// time to shrink.
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

// graphicsInfo returns cell geometry, serving a stale cached value while a
// refresh runs in the background rather than blocking the keypress on a
// pane.graphics.info round trip (only startup and a post-resize redraw wait).
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

// refreshInfoAsync re-reads geometry on a background goroutine (at most one
// at a time); a transient failure keeps the stale cached value rather than
// blanking the page the reader is looking at.
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

// showStatus repaints the text layer. The clear happens here, not at the
// top of fullRedraw, so the pane isn't blank for the whole render-and-send window.
func (v *Viewer) showStatus(cols, rows int, right string) {
	v.term.ClearScreen()
	v.writeStatus(cols, rows, right)
}

// fullRedraw re-queries geometry, renders the current page, sends it, and
// repaints the status line. Every failure path still shows a status message
// instead of leaving the pane blank.
func (v *Viewer) fullRedraw() {
	if v.browser != nil {
		v.drawBrowser()
		return
	}
	if v.helping {
		v.drawHelp()
		return
	}
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
	rasterW, rasterH := clampRaster(canvasW, canvasH)

	t0 := time.Now()
	frame, err := v.renderer.RenderPage(v.page, rasterW, rasterH)
	renderMS := msSince(t0)
	tr.mark("render")
	if err != nil {
		v.log("render page=%d failed: %v", v.page+1, err)
		v.showStatus(cols, rows, "render 失敗，見 viewer.log")
		return
	}

	// A clamped raster is smaller than the pane, so placement describes the
	// full-size box and lets herdr scale the image back up into it.
	dispW, dispH := frame.Width, frame.Height
	if rasterW != canvasW || rasterH != canvasH {
		dispW, dispH = fitBox(frame.Width, frame.Height, canvasW, canvasH)
	}
	placement := computePlacement(cols, drawRows, info.CellWidthPx, info.CellHeightPx, dispW, dispH)

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

	v.prefetchNeighbours(rasterW, rasterH)
}

// prefetchNeighbours warms the pages either side of the current one on a
// background goroutine after the current page is on screen, turning the
// next flip's RenderPage into a cache hit. Pages to warm are chosen up
// front so the goroutine never races the main loop's viewer state.
func (v *Viewer) prefetchNeighbours(rasterW, rasterH int) {
	order := []int{v.page + 1, v.page - 1}
	if v.lastDelta < 0 {
		order = []int{v.page - 1, v.page + 1}
	}
	var want []int
	for _, p := range order {
		if p >= 0 && p < v.renderer.PageCount() && !v.renderer.Cached(p, rasterW, rasterH) {
			want = append(want, p)
		}
	}
	if len(want) == 0 {
		return
	}
	r := v.renderer
	// Captured, not read inside the goroutine: by the time it stores, the
	// pane may have moved on to another document.
	doc := v.docGen
	encodeToo := v.transport == TransportStreamPNG
	v.startWork(func() {
		for _, p := range want {
			frame, err := r.RenderPage(p, rasterW, rasterH)
			if err != nil {
				continue
			}
			if !encodeToo {
				continue
			}
			// Claim the slot before the encode, not after: claiming after
			// means two prefetchers can both spend the encode and only then
			// discover one of them was redundant.
			if !v.reservePNG(doc, p, frame.Width, frame.Height) {
				continue
			}
			enc, eerr := v.encodePage(frame)
			if eerr != nil {
				v.releasePNG(doc, p, frame.Width, frame.Height)
				continue
			}
			v.storePNGAt(doc, p, frame, enc)
		}
	})
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
	mid := fmt.Sprintf("頁 %d/%s  %s", v.page+1, total, readingHelp.StatusHints())
	line := ui.StatusLine(name, mid, right, cols)
	v.term.WriteString(ui.RenderStatusLine(rows, line))
}

// errorStatus turns a herdr *Error into an actionable status-line message
// by switching on error.code, rather than a single generic failure string.
func (v *Viewer) errorStatus(err error) string {
	var he *herdr.Error
	if errors.As(err, &he) {
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
	if errors.Is(err, render.ErrTooLarge) || errors.Is(err, herdr.ErrFrameTooLarge) {
		return "此頁太大，未送出"
	}
	return "render 失敗，見 viewer.log"
}

// sendFrame pushes one rendered page via the configured transport, falling
// back to the short-lived graphics.set PNG path if the stream is unavailable.
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
		// A frame refused for its size says nothing about the connection, so
		// the stream stays open and the next page gets a fresh try on it.
		if errors.Is(err, herdr.ErrFrameTooLarge) || errors.Is(err, render.ErrTooLarge) {
			v.log("page=%d frame too large for the stream, not sent: %v", v.page+1, err)
			return strategy, sentBytes, err
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
		serr := v.stream.SendFrame(frame.RGBA, frame.Width, frame.Height, placement)
		if serr == nil {
			return "stream-rgba", len(frame.RGBA), nil
		}
		if !errors.Is(serr, herdr.ErrFrameTooLarge) {
			return "stream-rgba", len(frame.RGBA), serr
		}
		// Uncompressed pixels overflow the frame limit long before a
		// compressed page does, so the same page goes out as PNG rather than
		// costing the session its stream.
		v.log("rgba frame %dx%d over the stream limit, sending as png", frame.Width, frame.Height)
		strategy, sentBytes, err = v.sendStreamPNG(frame, placement, tr)
		return "rgba->" + strategy, sentBytes, err

	case TransportDirect:
		path, werr := v.writeFrameFile(frame)
		tr.mark("file_write")
		if werr != nil {
			return "direct", 0, werr
		}
		// Unlike inline frames, herdr acknowledges every file frame (even
		// success), so this measures the server actually taking it.
		if serr := v.stream.SendFile("rgba", path, frame.Width, frame.Height, placement); serr != nil {
			return "direct", len(frame.RGBA), serr
		}
		return "direct", len(frame.RGBA), nil

	default: // TransportStreamPNG
		return v.sendStreamPNG(frame, placement, tr)
	}
}

// sendStreamPNG encodes the page and pushes it on the open stream, sending
// the small stand-in first when progressive refinement is on.
func (v *Viewer) sendStreamPNG(frame *render.Frame, placement herdr.Placement, tr *traceSpan) (strategy string, sentBytes int, err error) {
	// A prefetch cache hit is the common case while reading in one
	// direction, keeping the colour-depth ladder off the keypress path.
	page, hit := v.lookupPNG(frame)
	if !hit {
		var eerr error
		page, eerr = v.encodePage(frame)
		if eerr != nil {
			tr.mark("png_encode")
			return "stream-png", 0, eerr
		}
		v.storePNGAt(v.docGen, v.page, frame, page)
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

// encodePage runs the colour ladder, plus the preview encode when
// progressive refinement is on.
func (v *Viewer) encodePage(frame *render.Frame) (encodedPage, error) {
	full, err := render.EncodeAdaptiveLimited(frame, v.maxFrameBytes, herdr.MaxInlineFrameBytes)
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

// sendEncoded pushes one encoded frame at the placement computed for the
// full-size page: herdr draws into the placement's cell box regardless of
// the image's own pixel size, so a smaller preview still fills the same area.
func (v *Viewer) sendEncoded(st frameSink, enc render.Encoded, placement herdr.Placement) error {
	return st.SendInline("png", enc.Payload, enc.Width, enc.Height, placement)
}

// scheduleRefine queues the full-resolution frame behind the preview. At
// most one refinement is ever pending: a faster reader replaces the payload
// and resets the timer instead of stacking a goroutine per page. The stream
// pointer is captured up front so a concurrent fallback to graphics.set
// can't race this field.
func (v *Viewer) scheduleRefine(full render.Encoded, placement herdr.Placement) {
	v.workMu.Lock()
	defer v.workMu.Unlock()
	if v.quitting {
		return
	}
	v.pendingRefine = &pendingRefine{
		gen:       v.redrawGen.Load(),
		full:      full,
		placement: placement,
		stream:    v.stream,
	}
	if v.refineTimer == nil {
		v.refineTimer = time.AfterFunc(refineDelay, v.sendRefine)
		return
	}
	v.refineTimer.Stop()
	v.refineTimer.Reset(refineDelay)
}

// sendRefine runs on the refine timer; the reader having moved on since the
// preview, or a shutdown having started, cancels it.
func (v *Viewer) sendRefine() {
	v.workMu.Lock()
	p := v.pendingRefine
	v.pendingRefine = nil
	if p == nil || p.stream == nil || v.quitting || v.redrawGen.Load() != p.gen {
		v.workMu.Unlock()
		return
	}
	v.work.Add(1)
	v.workMu.Unlock()
	defer v.work.Done()

	if err := v.sendEncoded(p.stream, p.full, p.placement); err != nil {
		v.log("refine send failed: %v", err)
		return
	}
	v.log("refine sent strategy=%s bytes=%d", p.full.Strategy, len(p.full.Payload))
}

func (v *Viewer) lookupPNG(frame *render.Frame) (encodedPage, bool) {
	v.pngMu.Lock()
	defer v.pngMu.Unlock()
	e, ok := v.pngCache[pngKey{v.docGen, v.page, frame.Width, frame.Height}]
	return e, ok
}

// reservePNG claims a cache slot before the encode starts, so a second
// prefetch for the same frame skips the work rather than duplicating it.
func (v *Viewer) reservePNG(doc uint64, page, w, h int) bool {
	v.pngMu.Lock()
	defer v.pngMu.Unlock()
	k := pngKey{doc, page, w, h}
	if _, done := v.pngCache[k]; done {
		return false
	}
	if v.pngPending[k] {
		return false
	}
	if v.pngPending == nil {
		v.pngPending = make(map[pngKey]bool)
	}
	v.pngPending[k] = true
	return true
}

// releasePNG drops a claim whose encode did not produce anything.
func (v *Viewer) releasePNG(doc uint64, page, w, h int) {
	v.pngMu.Lock()
	delete(v.pngPending, pngKey{doc, page, w, h})
	v.pngMu.Unlock()
}

func (v *Viewer) storePNGAt(doc uint64, page int, frame *render.Frame, enc encodedPage) {
	v.pngMu.Lock()
	defer v.pngMu.Unlock()
	if v.pngCache == nil {
		v.pngCache = make(map[pngKey]encodedPage)
	}
	k := pngKey{doc, page, frame.Width, frame.Height}
	delete(v.pngPending, k)
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
// file_frame_directory and returns its path. The filename rotates over a
// small ring rather than being reused, since herdr keeps the leased file
// open and re-validates it while drawing — overwriting an in-flight path
// could swap pixels mid-draw.
func (v *Viewer) writeFrameFile(frame *render.Frame) (string, error) {
	if v.frameDir == "" {
		return "", fmt.Errorf("no file_frame_directory for direct transport")
	}
	v.frameSeq++
	name := fmt.Sprintf("herdr-pdf-%d.rgba", v.frameSeq%4)
	path := filepath.Join(v.frameDir, name)
	// Truncate-and-write, not write-then-rename: herdr refuses symlinks and
	// requires a direct child of the source directory; a rename would leave
	// the old inode leased.
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

// sendViaSet is the PNG-over-graphics.set fallback: it shrinks and retries
// until a candidate fits under setMaxBytes, and reports failure rather than
// sending an oversized image after exhausting its attempts.
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
			// image_too_large despite our size check: server compares
			// decoded bytes, which can differ from our PNG byte count.
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

	return "set", fmt.Errorf("此頁太大：縮放 %d 次後仍超過 %dKB 上限，未送出", setMaxTries, setMaxBytes/1024)
}

// planShrink computes the next (scale, width, height) to retry at, or
// ok=false once attempts are exhausted. Kept pure (no I/O) so the
// termination condition is unit-testable without a live renderer.
func planShrink(attempt, baseW, baseH int, scale float64) (newScale float64, w, h int, ok bool) {
	if attempt >= setMaxTries-1 {
		return scale, 0, 0, false
	}
	newScale = scale * setShrinkPct
	w = maxInt(1, int(float64(baseW)*newScale))
	h = maxInt(1, int(float64(baseH)*newScale))
	return newScale, w, h, true
}

// usageScreen shows a plain-text message and waits for q/Ctrl-C/EOF, used
// for every "can't even start" case so the pane explains itself instead of a silent close.
func (v *Viewer) usageScreen(reason string) error {
	t := ui.New()
	if err := t.Enter(); err != nil {
		fmt.Fprintln(os.Stderr, "herdr-pdf:", reason)
		return fmt.Errorf("%s", reason)
	}
	defer t.Restore()

	t.ClearScreen()
	msg := "herdr PDF Viewer\r\n\r\n" + reason + "\r\n\r\n" +
		"用法: 設定環境變數 PDFVIEW_FILE=/path/to/file.pdf 後開啟本 pane，\r\n" +
		"或用 `herdr-pdf open /path/to/file.pdf`。\r\n\r\n按 q 離開。\r\n"
	t.WriteString("\x1b[1;1H" + msg)

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
