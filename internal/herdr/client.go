// Package herdr implements the client side of herdr's socket API:
// newline-delimited JSON over a Unix domain socket at $HERDR_SOCKET_PATH.
//
// The protocol has a hard constraint that shapes this whole package: the
// server handles exactly one request per connection (herdr src/api/server.rs:169
// calls read_initial_request_line once, with no loop after it) and then
// closes. So Call always dials fresh. The one exception is
// pane.graphics.stream, which hands the connection off to a different
// handler entirely (see stream.go) — that's why it isn't just another Call.
package herdr

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// SocketPath returns $HERDR_SOCKET_PATH, or the documented default if unset.
func SocketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

// Error represents herdr's top-level {"error":{"code","message"}} response
// shape. It is never nested inside "result" — G1/ARCH-review both confirm
// the Python viewer's dead code path that checked result.code was wrong;
// herdr always reports errors at the top level
// (api/schema/response.rs, app/api/responses.rs).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type rpcRequest struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// Call opens a fresh connection, writes exactly one request line, reads
// exactly one response line, and closes. result, if non-nil, receives the
// decoded "result" object. Returns the *Error as err when herdr reports a
// top-level error (so callers can type-assert to inspect .Code).
func Call(method string, params interface{}, result interface{}) error {
	conn, err := net.DialTimeout("unix", SocketPath(), 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", SocketPath(), err)
	}
	defer conn.Close()

	req := rpcRequest{ID: "herdr-pdf", Method: method, Params: params}
	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(conn, 64*1024)
	respLine, err := br.ReadString('\n')
	if err != nil && respLine == "" {
		return fmt.Errorf("read response: %w", err)
	}

	var resp rpcResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		return fmt.Errorf("decode response %q: %w", respLine, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode result %q: %w", resp.Result, err)
		}
	}
	return nil
}

// GraphicsInfo is the result of pane.graphics.info.
type GraphicsInfo struct {
	CellWidthPx        int    `json:"cell_width_px"`
	CellHeightPx       int    `json:"cell_height_px"`
	PaneVisible        bool   `json:"pane_visible"`
	FileFrameTransport string `json:"file_frame_transport"`
	FileFrameDirectory string `json:"file_frame_directory"`
	MaxLayersPerPane   int    `json:"max_layers_per_pane"`
}

// PaneGraphicsInfo fetches cell pixel geometry for paneID. Must be called
// fresh before every render — cell size is not stable across clients or
// even across calls to the same pane (observed 8x19 vs 15x38 px, ARCH-review
// "邊界案例矩陣"), so callers must never cache the result.
func PaneGraphicsInfo(paneID string) (*GraphicsInfo, error) {
	var info GraphicsInfo
	if err := Call("pane.graphics.info", map[string]string{"pane_id": paneID}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

type paneRect struct {
	PaneID string `json:"pane_id"`
	Rect   struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"rect"`
}

type layoutResult struct {
	Layout struct {
		Panes []paneRect `json:"panes"`
	} `json:"layout"`
}

// PaneCells returns this pane's current size in terminal cells (cols, rows)
// via pane.layout. TIOCGWINSZ on the pane's own stdout gives the same
// numbers without a socket round trip and is what the viewer actually uses
// day to day (see internal/ui/term.go); this is kept for callers (like the
// build's smoke test) that don't have a controlling tty of their own.
func PaneCells(paneID string) (cols, rows int, err error) {
	var lr layoutResult
	if err := Call("pane.layout", map[string]string{"pane_id": paneID}, &lr); err != nil {
		return 0, 0, err
	}
	for _, p := range lr.Layout.Panes {
		if p.PaneID == paneID {
			return p.Rect.Width, p.Rect.Height, nil
		}
	}
	return 0, 0, fmt.Errorf("pane %s not found in layout", paneID)
}

// Placement describes where a graphics layer sits in the pane's cell grid.
type Placement struct {
	ViewportCol int `json:"viewport_col"`
	ViewportRow int `json:"viewport_row"`
	GridCols    int `json:"grid_cols"`
	GridRows    int `json:"grid_rows"`
}

// GraphicsSet pushes a PNG-encoded page image via the short-lived
// pane.graphics.set call. This is the fallback path used only when the
// long-lived stream (see stream.go) fails or is unavailable, because it is
// capped at 512 KiB decoded (PANE_GRAPHICS_SET_MAX_BYTES) versus the
// stream's 16 MiB.
func GraphicsSet(paneID string, pngBytes []byte, width, height int, layerID string, placement Placement) error {
	params := map[string]interface{}{
		"pane_id":      paneID,
		"format":       "png",
		"image_width":  width,
		"image_height": height,
		"data_base64":  base64.StdEncoding.EncodeToString(pngBytes),
		"layer_id":     layerID,
		"z_index":      0,
		"placement":    placement,
	}
	return Call("pane.graphics.set", params, nil)
}

// GraphicsClear removes a layer. Errors are non-fatal to callers (best
// effort cleanup on exit).
func GraphicsClear(paneID, layerID string) error {
	return Call("pane.graphics.clear", map[string]string{"pane_id": paneID, "layer_id": layerID}, nil)
}
