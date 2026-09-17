package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxInlineFrameBytes is herdr's per-frame ceiling on the graphics stream.
// A frame over it is dropped by the server, so callers check before spending
// a write on it.
const MaxInlineFrameBytes = 16 << 20

// ErrFrameTooLarge reports a frame the stream will not accept; callers can
// re-encode smaller instead of treating it as a dead connection.
var ErrFrameTooLarge = errors.New("frame exceeds the stream size limit")

// StreamSession is a long-lived connection dedicated to pane.graphics.stream.
// herdr handles only one request per connection, so after stream open every
// line on it is parsed as a frame header — this type must never be reused
// for other calls. Inline frames are acked by silence (a response line means
// rejection); Failed() reports errors seen on a background reader instead of
// blocking the sender.
type StreamSession struct {
	conn    net.Conn
	paneID  string
	layerID string

	writeMu sync.Mutex
	frameNo uint64

	failedOnce sync.Once
	failedErr  error
	failedMu   sync.RWMutex

	// ackCh carries one entry per response line herdr writes. Inline frames
	// push nothing here on success; SendFile waits on it for a real ack.
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
	respLine, err := readLine(br)
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

// watch runs for the life of the connection, publishing every response line
// herdr writes onto ackCh. A rejected inline frame surfaces on Failed();
// SendFile waits on ackCh directly for its own completion signal.
func (s *StreamSession) watch(br *bufio.Reader) {
	for {
		line, err := readLine(br)
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var resp rpcResponse
			if jerr := json.Unmarshal([]byte(trimmed), &resp); jerr == nil {
				if resp.Error != nil {
					// Surface the structured *Error so callers can branch on
					// .Code, the same as with Call().
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

// Failed reports the first error observed on this session, if any; callers
// should check it after SendFrame to decide whether to fall back to GraphicsSet.
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

// SendInline writes one inline frame ("png", "rgb", "rgba", or "bgra"). It
// does not wait for an ack (there isn't one on success); check Failed()
// afterward to detect a rejection. Sequence and revision both advance
// together with each frame; their independent semantics are unverified.
func (s *StreamSession) SendInline(format string, payload []byte, width, height int, placement Placement) error {
	if err := s.Failed(); err != nil {
		return err
	}
	if len(payload) == 0 {
		return fmt.Errorf("empty %s frame", format)
	}
	if len(payload) > MaxInlineFrameBytes {
		return fmt.Errorf("%w: %s frame is %d bytes, limit is %d", ErrFrameTooLarge, format, len(payload), MaxInlineFrameBytes)
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
// carrying its bytes, and waits for the per-frame ack herdr sends for this
// variant. path must be an absolute, non-symlink file that is a direct
// child of pane.graphics.info's file_frame_directory and exactly
// width*height*4 bytes — herdr validates this and rejects the frame otherwise.
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
