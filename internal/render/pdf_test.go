package render

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pdfiumerr "github.com/klippa-app/go-pdfium/errors"
)

func TestRenderOnceCollapsesConcurrentCallsForOneKey(t *testing.T) {
	r := &Renderer{cacheCap: 6}
	var calls atomic.Int32
	key := cacheKey{page: 3, w: 800, h: 600}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame, err := r.renderOnce(key, func() (*Frame, error) {
				calls.Add(1)
				time.Sleep(20 * time.Millisecond)
				return &Frame{Width: 800, Height: 600}, nil
			})
			if err != nil {
				t.Errorf("renderOnce: %v", err)
				return
			}
			if frame.Width != 800 {
				t.Errorf("frame = %dx%d, want 800x600", frame.Width, frame.Height)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("rasterised %d times for one key, want exactly 1", got)
	}
}

func TestRenderOnceStillDedupesAcrossDifferentKeys(t *testing.T) {
	r := &Renderer{cacheCap: 6}
	var calls atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		for j := 0; j < 6; j++ {
			wg.Add(1)
			go func(page int) {
				defer wg.Done()
				_, _ = r.renderOnce(cacheKey{page: page, w: 100, h: 100}, func() (*Frame, error) {
					calls.Add(1)
					time.Sleep(10 * time.Millisecond)
					return &Frame{Width: 100, Height: 100}, nil
				})
			}(i)
		}
	}
	wg.Wait()

	if got := calls.Load(); got != 4 {
		t.Errorf("rasterised %d times for 4 distinct keys, want 4", got)
	}
}

func TestRenderOnceDoesNotCacheAFailure(t *testing.T) {
	r := &Renderer{cacheCap: 6}
	key := cacheKey{page: 0, w: 10, h: 10}

	if _, err := r.renderOnce(key, func() (*Frame, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Fatal("expected the render error to surface")
	}

	var calls int
	frame, err := r.renderOnce(key, func() (*Frame, error) {
		calls++
		return &Frame{Width: 10, Height: 10}, nil
	})
	if err != nil {
		t.Fatalf("retry after a failure: %v", err)
	}
	if calls != 1 || frame == nil {
		t.Errorf("a failed render must not be cached; retry ran %d times", calls)
	}
}

func TestRenderOnceEvictsPastTheCacheCap(t *testing.T) {
	r := &Renderer{cacheCap: 2}
	for p := 0; p < 5; p++ {
		if _, err := r.renderOnce(cacheKey{page: p, w: 1, h: 1}, func() (*Frame, error) {
			return &Frame{Width: 1, Height: 1}, nil
		}); err != nil {
			t.Fatalf("renderOnce: %v", err)
		}
	}
	if len(r.cache) != 2 {
		t.Errorf("cache holds %d frames, want cap 2", len(r.cache))
	}
	if len(r.inflight) != 0 {
		t.Errorf("inflight map leaked %d entries", len(r.inflight))
	}
}

func TestClassifyOpenError(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		// pdfium's own sentinels, returned unwrapped by the WebAssembly backend.
		{"sentinel password", pdfiumerr.ErrPassword, ErrEncrypted},
		{"sentinel security", pdfiumerr.ErrSecurity, ErrEncrypted},
		{"sentinel format", pdfiumerr.ErrFormat, ErrCorrupt},
		{"sentinel wrapped", fmt.Errorf("open document: %w", pdfiumerr.ErrPassword), ErrEncrypted},
		{"sentinel unclassified", pdfiumerr.ErrPage, nil},
		// Text fallback, for a backend that reports the failure some other way.
		{"text password", errors.New("4: invalid password"), ErrEncrypted},
		{"text encryption", errors.New("5: invalid encryption"), ErrEncrypted},
		{"text bad format", errors.New("3: incorrect format"), ErrCorrupt},
		{"text format error", errors.New("pdfium: Format error"), ErrCorrupt},
		{"text unclassified", errors.New("something else entirely"), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyOpenError(c.in)
			if c.want == nil {
				if errors.Is(got, ErrEncrypted) || errors.Is(got, ErrCorrupt) {
					t.Errorf("%v was classified, want it left alone", got)
				}
				return
			}
			if !errors.Is(got, c.want) {
				t.Errorf("classify(%v) = %v, want it to wrap %v", c.in, got, c.want)
			}
			// pdfium's own wording must survive for the log.
			if !strings.Contains(got.Error(), c.in.Error()) {
				t.Errorf("classified error %q dropped the original detail %q", got, c.in)
			}
		})
	}
}

func TestClassifyOpenErrorPrefersSentinelOverText(t *testing.T) {
	// A sentinel whose text would fall into the other bucket must still be
	// classified by identity, not by its wording.
	misleading := fmt.Errorf("incorrect format: %w", pdfiumerr.ErrPassword)
	if got := classifyOpenError(misleading); !errors.Is(got, ErrEncrypted) {
		t.Errorf("classify = %v, want the sentinel to win over the message text", got)
	}
}
