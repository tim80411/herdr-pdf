package viewer

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tim80411/herdr-pdf/internal/herdr"
	"github.com/tim80411/herdr-pdf/internal/render"
	"github.com/tim80411/herdr-pdf/internal/ui"
)

func keyRune(r rune) ui.KeyEvent {
	return ui.KeyEvent{Key: ui.KeyRune, Rune: r}
}

func TestCeilDiv(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{1061, 38, 28}, // 1061/38 = 27.9 -> 28
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
	// cell=15x38, canvas image 750x1061, pane 50 cols x 55 rows (54 draw
	// rows after status line).
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
	// planShrink must report ok=false once the attempt budget is
	// exhausted, so the caller can give up and send nothing instead of an
	// oversized image.
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

// fakeScreen stands in for a real terminal so the quit path can be driven
// without a tty.
func fakeScreen(t *testing.T) (*ui.Terminal, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	term := ui.NewForOutput(&buf)
	if err := term.Enter(); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	buf.Reset()
	return term, &buf
}

func assertRestored(t *testing.T, out string) {
	t.Helper()
	for _, seq := range []string{"\x1b[?1049l", "\x1b[?25h", "\x1b[?7h"} {
		if !strings.Contains(out, seq) {
			t.Errorf("terminal was not restored: %q missing from %q", seq, out)
		}
	}
}

func TestSignalExitRestoresTerminalAndReportsStatus(t *testing.T) {
	term, buf := fakeScreen(t)
	v := &Viewer{term: term}

	err := v.signalExit(syscall.SIGTERM)

	var sigErr *ExitSignal
	if !errors.As(err, &sigErr) {
		t.Fatalf("err = %v (%T), want *ExitSignal", err, err)
	}
	if got := sigErr.ExitCode(); got != 128+int(syscall.SIGTERM) {
		t.Errorf("ExitCode() = %d, want %d", got, 128+int(syscall.SIGTERM))
	}
	assertRestored(t, buf.String())
}

func TestSignalExitCodesPerSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		term, buf := fakeScreen(t)
		v := &Viewer{term: term}
		err := v.signalExit(sig)
		var sigErr *ExitSignal
		if !errors.As(err, &sigErr) {
			t.Fatalf("%v: err = %v, want *ExitSignal", sig, err)
		}
		if got, want := sigErr.ExitCode(), 128+int(sig); got != want {
			t.Errorf("%v: ExitCode() = %d, want %d", sig, got, want)
		}
		assertRestored(t, buf.String())
	}
}

func TestQuitSignalsReachTheViewerInsteadOfKillingIt(t *testing.T) {
	// Registering the same set Run() uses and delivering each one proves the
	// process survives them and can run its own teardown.
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, quitSignals...)
	defer signal.Stop(ch)

	for _, want := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		if err := syscall.Kill(os.Getpid(), want); err != nil {
			t.Fatalf("kill %v: %v", want, err)
		}
		select {
		case got := <-ch:
			if got != want {
				t.Errorf("received %v, want %v", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%v was never delivered to the viewer's channel", want)
		}
	}
}

func TestShutdownWaitsForBackgroundWorkThenRestores(t *testing.T) {
	baseline := runtime.NumGoroutine()
	term, buf := fakeScreen(t)
	v := &Viewer{term: term}

	// A burst of page flips: each queues a refinement, replacing the last.
	for i := 0; i < 50; i++ {
		v.redrawGen.Add(1)
		v.scheduleRefine(render.Encoded{Strategy: "full", Payload: []byte{byte(i)}}, herdr.Placement{})
	}
	// Plus background jobs standing in for prefetch and resize work.
	var finished atomic.Int32
	for i := 0; i < 10; i++ {
		v.startWork(func() {
			time.Sleep(20 * time.Millisecond)
			finished.Add(1)
		})
	}

	v.shutdown()

	if got := finished.Load(); got != 10 {
		t.Errorf("shutdown returned with %d/10 background jobs done; it must wait for all of them", got)
	}
	assertRestored(t, buf.String())

	v.workMu.Lock()
	pending := v.pendingRefine
	v.workMu.Unlock()
	if pending != nil {
		t.Error("shutdown left a refinement queued")
	}

	// Nothing new may start once the stream is on its way out.
	v.startWork(func() { t.Error("startWork ran a job after shutdown") })
	v.work.Wait()

	waitForGoroutines(t, baseline)
}

// waitForGoroutines polls until the count drops back to baseline, since a
// finished goroutine is not retired the instant its work returns.
func waitForGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("goroutines = %d, want back down to %d", got, baseline)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScheduleRefineKeepsOnlyTheLatestPending(t *testing.T) {
	v := &Viewer{}
	var firstTimer *time.Timer

	for i := 0; i < 50; i++ {
		v.redrawGen.Add(1)
		v.scheduleRefine(render.Encoded{Strategy: strconv.Itoa(i), Payload: []byte{byte(i)}}, herdr.Placement{})
		if i == 0 {
			v.workMu.Lock()
			firstTimer = v.refineTimer
			v.workMu.Unlock()
		}
	}

	v.workMu.Lock()
	timer, pending := v.refineTimer, v.pendingRefine
	v.workMu.Unlock()

	if timer != firstTimer {
		t.Error("a burst of page flips must reuse one timer, not arm a new one per page")
	}
	if pending == nil {
		t.Fatal("no refinement queued")
	}
	if pending.full.Strategy != "49" {
		t.Errorf("queued payload is %q, want only the last page's %q", pending.full.Strategy, "49")
	}
	v.shutdown()
}

func TestSendRefineSkipsAStalePage(t *testing.T) {
	v := &Viewer{}
	v.workMu.Lock()
	v.pendingRefine = &pendingRefine{gen: v.redrawGen.Load(), full: render.Encoded{Payload: []byte{1}}}
	v.workMu.Unlock()

	// The reader flips to another page before the timer fires.
	v.redrawGen.Add(1)
	v.sendRefine() // must return without touching the nil stream

	v.workMu.Lock()
	pending := v.pendingRefine
	v.workMu.Unlock()
	if pending != nil {
		t.Error("a cancelled refinement must be dropped, not left queued")
	}
}

func TestReservePNGClaimsASlotBeforeEncoding(t *testing.T) {
	v := &Viewer{}
	if !v.reservePNG(2, 800, 600) {
		t.Fatal("the first claim on a free slot must succeed")
	}
	if v.reservePNG(2, 800, 600) {
		t.Error("a second prefetch must see the claim and skip the encode")
	}
	if !v.reservePNG(2, 400, 300) {
		t.Error("a different frame size is a different slot and must be claimable")
	}

	// A failed encode releases the claim so the page can be retried.
	v.releasePNG(2, 800, 600)
	if !v.reservePNG(2, 800, 600) {
		t.Error("a released claim must be reusable")
	}

	// Storing the result clears the claim and blocks further encodes.
	v.storePNGAt(2, &render.Frame{Width: 800, Height: 600}, encodedPage{})
	if v.reservePNG(2, 800, 600) {
		t.Error("an already-encoded frame must not be claimable")
	}
	v.pngMu.Lock()
	leaked := len(v.pngPending)
	v.pngMu.Unlock()
	if leaked != 1 { // only the 400x300 claim, never released
		t.Errorf("pngPending holds %d claims, want 1", leaked)
	}
}

func TestReservePNGUnderConcurrentPrefetch(t *testing.T) {
	v := &Viewer{}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v.reservePNG(7, 1000, 1400) {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := claims.Load(); got != 1 {
		t.Errorf("%d goroutines claimed the same slot, want exactly 1", got)
	}
}

func TestClampRasterCapsResolutionAndKeepsAspect(t *testing.T) {
	cases := []struct{ w, h int }{
		{1600, 1200},   // under both caps
		{4000, 3000},   // over width
		{1000, 9000},   // over height
		{12000, 16000}, // far over both
	}
	for _, c := range cases {
		w, h := clampRaster(c.w, c.h)
		if w > maxRasterW || h > maxRasterH {
			t.Errorf("clampRaster(%d,%d) = %dx%d, over the %dx%d cap", c.w, c.h, w, h, maxRasterW, maxRasterH)
		}
		if w < 1 || h < 1 {
			t.Errorf("clampRaster(%d,%d) = %dx%d, must stay positive", c.w, c.h, w, h)
		}
		if c.w <= maxRasterW && c.h <= maxRasterH {
			if w != c.w || h != c.h {
				t.Errorf("clampRaster(%d,%d) = %dx%d, must pass an in-range canvas through", c.w, c.h, w, h)
			}
			continue
		}
		in := float64(c.w) / float64(c.h)
		out := float64(w) / float64(h)
		if math.Abs(in-out)/in > 0.01 {
			t.Errorf("clampRaster(%d,%d) = %dx%d changed the aspect ratio %.4f -> %.4f", c.w, c.h, w, h, in, out)
		}
	}
}

func TestFitBoxScalesBackUpToTheCanvas(t *testing.T) {
	// A clamped raster must still be placed across the whole pane.
	w, h := fitBox(2400, 1800, 4000, 3000)
	if w != 4000 || h != 3000 {
		t.Errorf("fitBox = %dx%d, want 4000x3000", w, h)
	}
	// Never exceed the box on the constrained axis.
	w, h = fitBox(2400, 3200, 4000, 3000)
	if w > 4000 || h > 3000 {
		t.Errorf("fitBox = %dx%d, must fit inside 4000x3000", w, h)
	}
}

func TestOpenLogIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.log")
	f := openLog(fresh)
	if f == nil {
		t.Fatal("openLog returned nil for a new file")
	}
	f.Close()
	if fi, err := os.Stat(fresh); err != nil {
		t.Fatalf("stat: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("new log mode = %04o, want 0600", perm)
	}

	// A log left behind by an older run must be tightened, not trusted.
	existing := filepath.Join(dir, "existing.log")
	if err := os.WriteFile(existing, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f = openLog(existing)
	if f == nil {
		t.Fatal("openLog returned nil for an existing file")
	}
	f.Close()
	if fi, err := os.Stat(existing); err != nil {
		t.Fatalf("stat: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("existing log mode = %04o, want 0600", perm)
	}
}

func TestOpenFailureMessageDistinguishesCauses(t *testing.T) {
	path := "/docs/report.pdf"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"encrypted", fmt.Errorf("%w: 4: invalid password", render.ErrEncrypted), "需要密碼"},
		{"corrupt", fmt.Errorf("%w: 3: incorrect format", render.ErrCorrupt), "損毀"},
		{"other", errors.New("5: unknown"), "無法開啟"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := openFailureMessage(c.err, path)
			if !strings.Contains(got, c.want) {
				t.Errorf("message = %q, want it to mention %q", got, c.want)
			}
			if !strings.Contains(got, "report.pdf") {
				t.Errorf("message = %q, want it to name the file", got)
			}
		})
	}
}

func TestErrorStatusUsesWrappedHerdrCodes(t *testing.T) {
	v := &Viewer{}
	// A herdr error reached through a wrapper must still be classified.
	wrapped := fmt.Errorf("stream send failed: %w", &herdr.Error{Code: "image_too_large", Message: "nope"})
	if got := v.errorStatus(wrapped); got != "此頁太大，見 viewer.log" {
		t.Errorf("errorStatus = %q, want the image_too_large message", got)
	}
	if got := v.errorStatus(fmt.Errorf("encode: %w", render.ErrTooLarge)); !strings.Contains(got, "太大") {
		t.Errorf("errorStatus = %q, want an over-size message for render.ErrTooLarge", got)
	}
	if got := v.errorStatus(fmt.Errorf("send: %w", herdr.ErrFrameTooLarge)); !strings.Contains(got, "太大") {
		t.Errorf("errorStatus = %q, want an over-size message for herdr.ErrFrameTooLarge", got)
	}
}

// fakeStream records what the viewer sends and applies the same size limit
// the real stream enforces, so the send path can be driven without a socket.
type fakeStream struct {
	mu      sync.Mutex
	limit   int // 0 means herdr.MaxInlineFrameBytes
	sendErr error

	inlineFormats []string
	inlineBytes   []int
	frameAttempts int
	closes        int
}

func (f *fakeStream) cap() int {
	if f.limit > 0 {
		return f.limit
	}
	return herdr.MaxInlineFrameBytes
}

func (f *fakeStream) SendInline(format string, payload []byte, width, height int, placement herdr.Placement) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	if len(payload) > f.cap() {
		return fmt.Errorf("%w: %s frame is %d bytes, limit is %d", herdr.ErrFrameTooLarge, format, len(payload), f.cap())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inlineFormats = append(f.inlineFormats, format)
	f.inlineBytes = append(f.inlineBytes, len(payload))
	return nil
}

func (f *fakeStream) SendFrame(pix []byte, width, height int, placement herdr.Placement) error {
	f.mu.Lock()
	f.frameAttempts++
	f.mu.Unlock()
	return f.SendInline("rgba", pix, width, height, placement)
}

func (f *fakeStream) SendFile(format, path string, width, height int, placement herdr.Placement) error {
	return f.sendErr
}

func (f *fakeStream) Failed() error { return nil }

func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeStream) snapshot() ([]string, []int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inlineFormats...), append([]int(nil), f.inlineBytes...), f.frameAttempts, f.closes
}

// blankFrame builds a uniform RGBA page, which compresses to almost nothing
// so an over-limit raw buffer can be tested without a slow encode.
func blankFrame(w, h int) *render.Frame {
	pix := make([]byte, w*h*4)
	for i := range pix {
		pix[i] = 0xff
	}
	return &render.Frame{Width: w, Height: h, RGBA: pix}
}

func TestOversizedRGBAFallsBackToPNGOnTheSameStream(t *testing.T) {
	// A page rendered at the raster cap is ~29 MiB of RGBA, well past the
	// inline frame limit, so a tall pane hits this on its very first page.
	frame := blankFrame(maxRasterW, maxRasterH)
	if len(frame.RGBA) <= herdr.MaxInlineFrameBytes {
		t.Fatalf("a %dx%d raster is %d bytes, which no longer exceeds the %d frame limit; this test needs an over-limit frame",
			maxRasterW, maxRasterH, len(frame.RGBA), herdr.MaxInlineFrameBytes)
	}

	fs := &fakeStream{}
	v := &Viewer{transport: TransportStreamRGBA, stream: fs, maxFrameBytes: localFrameBudget}

	strategy, sent, err := v.sendFrame(frame, herdr.Placement{}, 80, 24, 8, 16, v.beginTrace())
	if err != nil {
		t.Fatalf("sendFrame: %v", err)
	}

	if v.stream == nil {
		t.Error("an over-size frame tore down the connection; the session is now stuck on graphics.set")
	}
	formats, sizes, attempts, closes := fs.snapshot()
	if closes != 0 {
		t.Errorf("stream was closed %d times, want 0", closes)
	}
	if attempts != 1 {
		t.Errorf("rgba send attempts = %d, want 1", attempts)
	}
	if len(formats) != 1 || formats[0] != "png" {
		t.Fatalf("inline sends = %v, want exactly one png frame", formats)
	}
	if sizes[0] > herdr.MaxInlineFrameBytes {
		t.Errorf("the png replacement is %d bytes, still over the limit", sizes[0])
	}
	if !strings.Contains(strategy, "png") {
		t.Errorf("strategy = %q, want it to report the png replacement", strategy)
	}
	if sent <= 0 {
		t.Errorf("sentBytes = %d, want the png payload size", sent)
	}
}

func TestSizeRefusalKeepsTheStreamAndSkipsGraphicsSet(t *testing.T) {
	// Point the socket at nothing: if the set fallback were reached it would
	// fail with a dial error instead of the size error asserted below.
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent.sock"))

	fs := &fakeStream{limit: 16} // refuses any real payload
	v := &Viewer{transport: TransportStreamPNG, stream: fs, maxFrameBytes: localFrameBudget}

	_, _, err := v.sendFrame(blankFrame(64, 64), herdr.Placement{}, 80, 24, 8, 16, v.beginTrace())
	if !errors.Is(err, herdr.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge (reaching graphics.set would give a dial error)", err)
	}
	if v.stream == nil {
		t.Error("a size refusal must not tear down the connection")
	}
	if _, _, _, closes := fs.snapshot(); closes != 0 {
		t.Errorf("stream was closed %d times, want 0", closes)
	}
	if got := v.errorStatus(err); !strings.Contains(got, "太大") {
		t.Errorf("status = %q, want it to tell the reader the page is too large", got)
	}
}

func TestConnectionErrorStillClosesTheStreamAndFallsBack(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent.sock"))

	fs := &fakeStream{sendErr: errors.New("write: broken pipe")}
	v := &Viewer{transport: TransportStreamPNG, stream: fs, maxFrameBytes: localFrameBudget}

	strategy, _, err := v.sendFrame(blankFrame(64, 64), herdr.Placement{}, 80, 24, 8, 16, v.beginTrace())
	if v.stream != nil {
		t.Error("a broken connection must be dropped, not reused")
	}
	if _, _, _, closes := fs.snapshot(); closes != 1 {
		t.Errorf("stream closed %d times, want 1", closes)
	}
	// The set fallback runs and fails on the absent socket, which is what
	// proves it was reached at all.
	if strategy != "set" || err == nil {
		t.Errorf("strategy = %q err = %v, want the graphics.set fallback to have been attempted", strategy, err)
	}
	if errors.Is(err, herdr.ErrFrameTooLarge) {
		t.Errorf("err = %v, want a connection failure rather than a size refusal", err)
	}
}

// The real session must satisfy the interface the send path is written against.
var _ frameSink = (*herdr.StreamSession)(nil)
