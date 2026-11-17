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

	"github.com/tim80411/herdr-pdf/internal/kgp"
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
	// Every value, including the removed socket transports and typos, lands
	// on the kitty transport; only a non-empty non-kitty value is "legacy".
	for in, legacy := range map[string]bool{
		"":            false,
		"kitty":       false,
		"  KITTY ":    false,
		"stream-png":  true,
		"stream-rgba": true,
		"  DIRECT  ":  true,
		"Set":         true,
		"nonsense":    true,
	} {
		got, gotLegacy := ParseTransport(in)
		if got != TransportKitty || gotLegacy != legacy {
			t.Errorf("ParseTransport(%q) = %q,%v want %q,%v", in, got, gotLegacy, TransportKitty, legacy)
		}
	}
}

func TestPNGCacheEvictsOldestAndKeysOnSize(t *testing.T) {
	v := &Viewer{}
	frame := func(w, h int) *render.Frame { return &render.Frame{Width: w, Height: h} }

	for p := 0; p < pngCacheCap+2; p++ {
		v.storePNGAt(0, p, frame(100, 200), encodedPage{full: render.Encoded{Payload: []byte{byte(p)}}})
	}
	if len(v.pngCache) != pngCacheCap {
		t.Fatalf("cache holds %d entries, want cap %d", len(v.pngCache), pngCacheCap)
	}
	if _, ok := v.pngCache[pngKey{0, 0, 100, 200}]; ok {
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
		v.scheduleRefine(render.Encoded{Strategy: "full", Payload: []byte{byte(i)}}, kgp.Placement{})
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
		v.scheduleRefine(render.Encoded{Strategy: strconv.Itoa(i), Payload: []byte{byte(i)}}, kgp.Placement{})
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
	if !v.reservePNG(0, 2, 800, 600) {
		t.Fatal("the first claim on a free slot must succeed")
	}
	if v.reservePNG(0, 2, 800, 600) {
		t.Error("a second prefetch must see the claim and skip the encode")
	}
	if !v.reservePNG(0, 2, 400, 300) {
		t.Error("a different frame size is a different slot and must be claimable")
	}

	// A failed encode releases the claim so the page can be retried.
	v.releasePNG(0, 2, 800, 600)
	if !v.reservePNG(0, 2, 800, 600) {
		t.Error("a released claim must be reusable")
	}

	// Storing the result clears the claim and blocks further encodes.
	v.storePNGAt(0, 2, &render.Frame{Width: 800, Height: 600}, encodedPage{})
	if v.reservePNG(0, 2, 800, 600) {
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
			if v.reservePNG(0, 7, 1000, 1400) {
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

func TestErrorStatusClassifiesFailures(t *testing.T) {
	v := &Viewer{}
	if got := v.errorStatus(fmt.Errorf("encode: %w", render.ErrTooLarge)); !strings.Contains(got, "太大") {
		t.Errorf("errorStatus = %q, want an over-size message for render.ErrTooLarge", got)
	}
	if got := v.errorStatus(&writeError{errors.New("broken pipe")}); !strings.Contains(got, "寫入終端") {
		t.Errorf("errorStatus = %q, want a terminal-write message", got)
	}
	for _, bad := range []string{"kitty_graphics", "pane.graphics"} {
		for _, err := range []error{render.ErrTooLarge, &writeError{errors.New("x")}, errors.New("y")} {
			if got := v.errorStatus(err); strings.Contains(got, bad) {
				t.Errorf("errorStatus(%v) = %q mentions the removed %s", err, got, bad)
			}
		}
	}
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

// lockedBuf is a pane output that tolerates the refine goroutine writing.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuf) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

// failWriter refuses every write.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestSendFrameWritesOneChunkedPNGAndReportsBytes(t *testing.T) {
	out := &lockedBuf{}
	v := &Viewer{out: out, maxFrameBytes: localFrameBudget}

	strategy, sent, err := v.sendFrame(blankFrame(64, 64), kgp.Placement{ViewportCol: 1, ViewportRow: 2, GridCols: 8, GridRows: 4}, v.beginTrace())
	if err != nil {
		t.Fatalf("sendFrame: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "\x1b[3;2H\x1b_Ga=T,f=100,t=d,i=1,c=8,r=4,C=1,q=2,m=") {
		t.Errorf("output starts %q, want cursor move then the transmit header", got[:min(len(got), 60)])
	}
	if strings.Contains(got, "a=d") {
		t.Errorf("first image must not delete anything: %q", got)
	}
	if !strings.Contains(strategy, "png") || sent <= 0 {
		t.Errorf("strategy=%q sent=%d, want a png strategy and its byte count", strategy, sent)
	}
	if len(v.live) != 1 || v.live[0] != 1 {
		t.Errorf("live ids = %v, want [1]", v.live)
	}
}

func TestPutImageUsesFreshIDsAndFreesThePreviousOneAfterwards(t *testing.T) {
	out := &lockedBuf{}
	v := &Viewer{out: out}
	enc := render.Encoded{Payload: []byte("abc")}

	for i := 0; i < 3; i++ {
		out.Reset()
		if err := v.putImage(enc, kgp.Placement{GridCols: 2, GridRows: 2}, nil); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		id := uint32(i + 1)
		tx := fmt.Sprintf("i=%d,", id)
		if !strings.Contains(got, tx) {
			t.Fatalf("image %d: output lacks %q: %q", i, tx, got)
		}
		del := fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", id-1)
		if i == 0 {
			if strings.Contains(got, "a=d") {
				t.Errorf("first image deleted something: %q", got)
			}
			continue
		}
		if !strings.HasSuffix(got, del) {
			t.Errorf("image %d: output should end by freeing the old id: %q", i, got)
		}
		if strings.Index(got, "a=T") > strings.Index(got, "a=d") {
			t.Errorf("image %d: old id freed before the new one was sent: %q", i, got)
		}
	}
	if len(v.live) != 1 || v.live[0] != 3 {
		t.Errorf("live ids = %v, want only the newest [3]", v.live)
	}
}

func TestPutImageWriteFailureKeepsOldIDsTracked(t *testing.T) {
	v := &Viewer{out: failWriter{}, live: []uint32{5}, nextID: 5}
	err := v.putImage(render.Encoded{Payload: []byte("x")}, kgp.Placement{}, nil)
	if err == nil {
		t.Fatal("want the write error")
	}
	if len(v.live) != 2 {
		t.Errorf("live = %v, want old and new ids both tracked so a later clear frees them", v.live)
	}
	_, _, sendErr := v.sendFrame(blankFrame(16, 16), kgp.Placement{}, v.beginTrace())
	if got := v.errorStatus(sendErr); !strings.Contains(got, "寫入終端") {
		t.Errorf("status = %q, want a write failure message", got)
	}
}

func TestClearImagesFreesEveryLiveID(t *testing.T) {
	out := &lockedBuf{}
	v := &Viewer{out: out, live: []uint32{4, 9}, nextID: 9}
	if err := v.clearImages(false); err != nil {
		t.Fatal(err)
	}
	want := "\x1b_Ga=d,d=I,i=4,q=2\x1b\\\x1b_Ga=d,d=I,i=9,q=2\x1b\\"
	if got := out.String(); got != want {
		t.Errorf("clearImages wrote %q, want %q", got, want)
	}
	if len(v.live) != 0 {
		t.Errorf("live = %v after clear", v.live)
	}
	out.Reset()
	if err := v.clearImages(false); err != nil || out.String() != "" {
		t.Errorf("clearing nothing wrote %q err=%v", out.String(), err)
	}
}

func TestStaleRefineIsNotDrawn(t *testing.T) {
	out := &lockedBuf{}
	v := &Viewer{out: out}
	err := v.putImage(render.Encoded{Payload: []byte("x")}, kgp.Placement{}, func() bool { return false })
	if !errors.Is(err, errStale) || out.String() != "" || v.nextID != 0 {
		t.Errorf("err=%v out=%q nextID=%d, want nothing written", err, out.String(), v.nextID)
	}
}

func TestSanePixelCell(t *testing.T) {
	for _, c := range []struct {
		name                 string
		pxW, pxH, cols, rows int
		wantW, wantH         int
		ok                   bool
	}{
		{"typical", 1990, 720, 199, 36, 10, 20, true},
		{"unreported", 0, 0, 199, 36, 0, 0, false},
		{"the nonsense seen on this machine", 1368, 2985, 199, 36, 0, 0, false},
		{"no grid", 100, 100, 0, 0, 0, 0, false},
	} {
		w, h, ok := sanePixelCell(c.pxW, c.pxH, c.cols, c.rows)
		if ok != c.ok || (ok && (w != c.wantW || h != c.wantH)) {
			t.Errorf("%s: got %d,%d,%v want %d,%d,%v", c.name, w, h, ok, c.wantW, c.wantH, c.ok)
		}
	}
}

func TestComputePlacementClampsToThePane(t *testing.T) {
	p := computePlacement(80, 23, 10, 20, 900, 500)
	if p.GridCols > 80 || p.GridRows > 23 {
		t.Errorf("box %dx%d exceeds the 80x23 pane", p.GridCols, p.GridRows)
	}
}

// readLog returns everything the viewer wrote to its log file.
func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestBrowseOnlyQuitWritesNoGraphics(t *testing.T) {
	// Nothing was ever drawn, so there is nothing to delete.
	term, buf := fakeScreen(t)
	out := &lockedBuf{}
	v := &Viewer{term: term, out: out}
	v.shutdown()
	if got := out.String(); got != "" {
		t.Errorf("shutdown wrote graphics escapes with no image drawn: %q", got)
	}
	assertRestored(t, buf.String())
}

func TestReadingQuitFreesItsImagesBeforeRestoring(t *testing.T) {
	term, _ := fakeScreen(t)
	out := &lockedBuf{}
	v := &Viewer{term: term, out: out, live: []uint32{2}, nextID: 2}
	v.shutdown()
	got := out.String()
	if !strings.Contains(got, "a=d,d=I,i=2,") || !strings.HasSuffix(got, "\x1b_Ga=d,d=A,q=2\x1b\\") {
		t.Errorf("shutdown wrote %q, want the live id and delete-all", got)
	}
}

func TestSignalExitFreesImages(t *testing.T) {
	term, _ := fakeScreen(t)
	out := &lockedBuf{}
	v := &Viewer{term: term, out: out, live: []uint32{1}, nextID: 1}
	_ = v.signalExit(syscall.SIGTERM)
	if !strings.Contains(out.String(), "a=d,d=I,i=1,") {
		t.Errorf("signal exit left the image behind: %q", out.String())
	}
}

func TestReadingHelpOverlayFreesTheImageAndRestartsGraphics(t *testing.T) {
	// The image composites above text, so it has to go while the key table
	// is up and come back when it closes.
	logPath := filepath.Join(t.TempDir(), "viewer.log")
	term, _ := fakeScreen(t)
	out := &lockedBuf{}

	v := &Viewer{
		term:          term,
		out:           out,
		opts:          Options{PaneID: "w1:p1", LogPath: logPath, PDFPath: "/tmp/x.pdf"},
		live:          []uint32{3},
		nextID:        3,
		graphicsReady: true,
	}
	v.logf = openLog(logPath)
	defer v.logf.Close()

	if quit := v.handleKey(ui.KeyEvent{Key: ui.KeyRune, Rune: '?'}); quit {
		t.Fatal("? quit the viewer")
	}
	if !v.helping {
		t.Fatal("? did not open the overlay")
	}
	if got := out.String(); !strings.Contains(got, "a=d,d=I,i=3,") {
		t.Errorf("the page image was not freed before the overlay: %q", got)
	}
	if len(v.live) != 0 || v.graphicsReady {
		t.Errorf("live=%v graphicsReady=%v, want both cleared", v.live, v.graphicsReady)
	}

	// Any key returns to the page and asks for a redraw.
	if quit := v.handleKey(ui.KeyEvent{Key: ui.KeyRune, Rune: 'q'}); quit {
		t.Fatal("q during the overlay quit the viewer; it should only close the table")
	}
	if v.helping {
		t.Fatal("the overlay stayed up")
	}
	if !v.needRedraw {
		t.Error("closing the overlay did not request a redraw")
	}
	if got := readLog(t, logPath); !strings.Contains(got, "transport=kitty") {
		t.Errorf("graphics were not restarted after the overlay; log said:\n%s", got)
	}
}

func TestLegacyTransportIsLoggedOnceAndIgnored(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "viewer.log")
	v := &Viewer{opts: Options{LogPath: logPath, LegacyTransport: "stream-rgba"}}
	v.logf = openLog(logPath)
	defer v.logf.Close()
	v.configureSession()
	v.configureSession()
	got := readLog(t, logPath)
	if n := strings.Count(got, "stream-rgba"); n != 1 {
		t.Errorf("legacy value logged %d times, want once:\n%s", n, got)
	}
	if v.transport != TransportKitty {
		t.Errorf("transport = %q, want kitty", v.transport)
	}
}

func TestReadingHelpOverlayDrawsTheKeyTableAndEOFStillQuits(t *testing.T) {
	term, buf := fakeScreen(t)
	v := &Viewer{term: term, opts: Options{PDFPath: "/tmp/x.pdf"}, helping: true}

	v.fullRedraw()
	out := buf.String()
	for _, want := range []string{"閱讀模式鍵位", "按任意鍵返回", "回到瀏覽模式"} {
		if !strings.Contains(out, want) {
			t.Errorf("the overlay is missing %q", want)
		}
	}

	if quit := v.handleKey(ui.KeyEvent{Key: ui.KeyEOF}); !quit {
		t.Error("EOF during the overlay did not quit")
	}
}

func TestReadingStatusHintsComeFromTheKeyTable(t *testing.T) {
	hints := readingHelp.StatusHints()
	for _, want := range []string{"j/k 翻頁", "? 說明", "q 離開"} {
		if !strings.Contains(hints, want) {
			t.Errorf("status hints %q are missing %q", hints, want)
		}
	}
	// The status row shares its width with the filename and the render
	// diagnostics, so the hint set stays short enough to leave the filename
	// readable at a normal split width.
	if w := ui.DisplayWidth(hints); w > 30 {
		t.Errorf("status hints are %d columns wide: %q — too wide for the row", w, hints)
	}
	// Keys documented only on the overlay must not leak into the row.
	for _, quiet := range []string{"o 瀏覽", "g/G 首尾"} {
		if strings.Contains(hints, quiet) {
			t.Errorf("hint %q belongs on the overlay, not the status row", quiet)
		}
	}
	// Every hint must correspond to a real binding in the table.
	for _, b := range readingHelp.Bindings {
		if b.Hint != "" && !strings.Contains(hints, b.Hint) {
			t.Errorf("binding %q declares hint %q but it is not in the status line", b.Keys, b.Hint)
		}
	}
}
