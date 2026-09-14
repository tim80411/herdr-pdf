package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StreamSession is a long-lived connection dedicated to pane.graphics.stream.
//
// This is NOT another Call: once pane.graphics.stream is accepted, the
// socket server hands the whole connection off to a dedicated frame-loop
// handler (herdr src/api/server.rs:200-202) and every subsequent line on it
// is parsed as a frame header — sending an info/layout request on this
// connection gets read as a malformed frame and the server rejects it
// (verified in G1: sending pane.graphics.info after a successful stream
// open comes back as {"error":{"code":"invalid_frame", ...}}). So this type
// must never be used for anything but frames, and geometry must come from
// separate Call()s on their own connections.
//
// Inline RGBA frames are acked by silence: herdr only writes a response
// line when a frame is *rejected*, then closes the connection
// (pane_graphics_stream.rs). A blocking read after every frame would hang
// forever on success, so acknowledgement is watched for on a background
// goroutine instead; Failed() reports the first error observed (a rejected
// frame, or the connection dropping) without the sender ever blocking on it.
//
// The layer is cleared server-side the moment this connection closes, so
// the session must be kept open for as long as the layer should stay drawn.
type StreamSession struct {
	conn    net.Conn
	paneID  string
	layerID string

	writeMu sync.Mutex
	frameNo uint64

	failedOnce sync.Once
	failedErr  error
	failedMu   sync.RWMutex

	// ackCh carries one entry per response line herdr writes on this
	// connection. Inline frames are acked by silence, so nothing is ever
	// pushed for them on the happy path; file frames (see SendFile) get a
	// response line for every frame, success included
	// (pane_graphics_stream.rs:253-254), which is the only place in this
	// protocol where a frame send has a real completion signal.
	ackCh chan error

	closed atomic.Bool
}

type frameFile struct {
	Path string `json:"path"`
}

type frameHeader struct {
	Format      string     `json:"format"`
	ImageWidth  int        `json:"image_width"`
	ImageHeight int        `json:"image_height"`
	DataLength  int        `json:"data_length,omitempty"`
	File        *frameFile `json:"file,omitempty"`
	Sequence    uint64     `json:"sequence"`
	Revision    uint64     `json:"revision"`
	Placement   Placement  `json:"placement"`
}

// OpenStream dials a fresh connection and issues pane.graphics.stream as
// its one and only request, then keeps the connection for frame sends.
func OpenStream(paneID, layerID string, zIndex int) (*StreamSession, error) {
	conn, err := net.DialTimeout("unix", SocketPath(), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", SocketPath(), err)
	}

	req := rpcRequest{
		ID:     "herdr-pdf-stream-open",
		Method: "pane.graphics.stream",
		Params: map[string]interface{}{
			"pane_id":  paneID,
			"layer_id": layerID,
			"z_index":  zIndex,
		},
	}
	line, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("marshal stream open: %w", err)
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write stream open: %w", err)
	}

	br := bufio.NewReaderSize(conn, 4096)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	respLine, err := br.ReadString('\n')
	if err != nil && respLine == "" {
		conn.Close()
		return nil, fmt.Errorf("read stream open response: %w", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("decode stream open response %q: %w", respLine, err)
	}
	if resp.Error != nil {
		conn.Close()
		return nil, resp.Error
	}
	conn.SetReadDeadline(time.Time{})
	conn.SetWriteDeadline(time.Time{})

	s := &StreamSession{conn: conn, paneID: paneID, layerID: layerID, ackCh: make(chan error, 8)}
	go s.watch(br)
	return s, nil
}

// watch runs for the life of the connection, publishing every response
// line herdr writes onto ackCh.
//
// Two frame kinds share this one reader. An inline frame is acked by
// silence — herdr writes a line only to *reject* it — so any line arriving
// while only inline frames are in flight is an error. A file frame is
// answered every time, success included, so SendFile can wait on ackCh for
// a genuine "the server has taken this frame" signal. Because the loop
// keeps running instead of returning after the first line (as it did when
// only inline frames existed), a rejected file frame no longer silently
// deafens the session to every later ack.
func (s *StreamSession) watch(br *bufio.Reader) {
	for {
		line, err := br.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var resp rpcResponse
			if jerr := json.Unmarshal([]byte(trimmed), &resp); jerr == nil {
				if resp.Error != nil {
					// Structured herdr error (e.g. image_too_large for an
					// oversized frame): surface the real *Error so callers
					// can branch on .Code the same way they do for Call().
					s.markFailed(resp.Error)
					s.pushAck(resp.Error)
					return
				}
				s.pushAck(nil)
			} else {
				rejected := fmt.Errorf("stream frame rejected by server: %s", trimmed)
				s.markFailed(rejected)
				s.pushAck(rejected)
				return
			}
		}
		if err != nil {
			if !s.closed.Load() {
				closedErr := fmt.Errorf("stream connection closed unexpectedly: %w", err)
				s.markFailed(closedErr)
				s.pushAck(closedErr)
			} else {
				s.pushAck(fmt.Errorf("stream closed"))
			}
			return
		}
	}
}

// pushAck never blocks: the channel is buffered, and an ack nobody is
// waiting for (every successful inline frame) must not wedge the reader.
func (s *StreamSession) pushAck(err error) {
	select {
	case s.ackCh <- err:
	default:
	}
}

func (s *StreamSession) markFailed(err error) {
	s.failedOnce.Do(func() {
		s.failedMu.Lock()
		s.failedErr = err
		s.failedMu.Unlock()
	})
}

// Failed reports the first error observed on this session, if any. Callers
// should check it after SendFrame to decide whether to fall back to
// GraphicsSet.
func (s *StreamSession) Failed() error {
	s.failedMu.RLock()
	defer s.failedMu.RUnlock()
	return s.failedErr
}

// SendFrame writes one tightly-packed RGBA frame (see render.TightRGBA).
// Kept as the RGBA-shaped convenience wrapper over SendInline, including
// the buffer-length check that only makes sense for an uncompressed format.
func (s *StreamSession) SendFrame(pix []byte, width, height int, placement Placement) error {
	if len(pix) != width*height*4 {
		return fmt.Errorf("frame buffer is %d bytes, want %d for %dx%d rgba", len(pix), width*height*4, width, height)
	}
	return s.SendInline("rgba", pix, width, height, placement)
}

// SendInline writes one inline frame in any format herdr's stream header
// accepts ("png", "rgb", "rgba", "bgra" — api/schema/panes.rs:371-376).
// It does not wait for an ack (there isn't one on success); check Failed()
// afterward, ideally after a short pause, to detect a rejection.
//
// "png" is the format that matters for latency. herdr never decodes it:
// expected_len() returns Ok(None) for Png (app/api/pane_graphics.rs:512-524)
// and kitty_graphics.rs:688-694 maps it straight to Kitty's own format code
// 100, so the compressed bytes travel untouched to the outer terminal and
// every downstream stage — the inline Vec copy, the whole-buffer
// DefaultHasher fingerprint, the uncompressed bincode wire message, the
// base64 expansion and the 3072-byte Kitty chunking — shrinks by the
// compression ratio instead of only the socket write.
//
// Sequence and revision are unverified as to their independent semantics
// (G1-report "遺留": stream_conflict and per-field meaning were never
// exercised against a rejection). This mirrors the one combination proven
// to work in the G1 spike: both fields advance together with each frame.
func (s *StreamSession) SendInline(format string, payload []byte, width, height int, placement Placement) error {
	if err := s.Failed(); err != nil {
		return err
	}
	if len(payload) == 0 {
		return fmt.Errorf("empty %s frame", format)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.frameNo++
	hdr := frameHeader{
		Format:      format,
		ImageWidth:  width,
		ImageHeight: height,
		DataLength:  len(payload),
		Sequence:    s.frameNo,
		Revision:    s.frameNo,
		Placement:   placement,
	}
	return s.writeFrame(hdr, payload)
}

// SendFile writes one frame that names an already-written file instead of
// carrying its bytes, and waits for the per-frame response herdr writes for
// this variant (pane_graphics_stream.rs:253-254).
//
// This is the only transport that can reach herdr's zero-copy path, and it
// is far narrower than it looks. app/api/pane_graphics.rs:312-315 gates the
// real `Layer::direct` on three conditions at once:
//
//	let primary = key.1 == PANE_GRAPHICS_PRIMARY_LAYER_ID;
//	let direct = self.direct_graphics_available && primary
//	    && params.format == PaneGraphicsFormat::Rgba;
//
// So a named layer such as "page" does NOT get the fast path: it falls into
// the else branch and pays lease.copy_rgba() (pane_graphics_files.rs:118-131),
// a full re-read of the file into a fresh Vec, on top of the disk write the
// plugin just did. Sending file frames to anything but the "primary" layer
// is therefore strictly slower than sending them inline, which is why the
// viewer only selects this transport together with PrimaryLayerID.
//
// path must be an absolute path to a regular, non-symlink file that is a
// direct child of pane.graphics.info's file_frame_directory and exactly
// width*height*4 bytes long; herdr validates all of that in lease()
// (pane_graphics_files.rs:64-81) and rejects the frame otherwise.
func (s *StreamSession) SendFile(format, path string, width, height int, placement Placement) error {
	if err := s.Failed(); err != nil {
		return err
	}

	s.writeMu.Lock()
	s.frameNo++
	hdr := frameHeader{
		Format:      format,
		ImageWidth:  width,
		ImageHeight: height,
		File:        &frameFile{Path: path},
		Sequence:    s.frameNo,
		Revision:    s.frameNo,
		Placement:   placement,
	}
	err := s.writeFrame(hdr, nil)
	s.writeMu.Unlock()
	if err != nil {
		return err
	}

	// Drain acks that arrived for earlier frames before waiting for this
	// one, so a stale entry can't be mistaken for this frame's response.
	select {
	case ackErr := <-s.ackCh:
		if ackErr != nil {
			return ackErr
		}
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out waiting for file frame ack")
	}
	return nil
}

// writeFrame emits the header line and any body. Callers hold writeMu.
func (s *StreamSession) writeFrame(hdr frameHeader, body []byte) error {
	hdrLine, err := json.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("marshal frame header: %w", err)
	}

	s.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer s.conn.SetWriteDeadline(time.Time{})
	if _, err := s.conn.Write(append(hdrLine, '\n')); err != nil {
		s.markFailed(err)
		return err
	}
	if len(body) > 0 {
		if _, err := s.conn.Write(body); err != nil {
			s.markFailed(err)
			return err
		}
	}
	return nil
}

// Close drops the connection, which causes herdr to clear this layer.
func (s *StreamSession) Close() error {
	s.closed.Store(true)
	return s.conn.Close()
}
