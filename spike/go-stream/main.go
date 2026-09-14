// Spike: render a PDF page with go-pdfium and push it into a herdr pane,
// either via pane.graphics.set (base64 PNG) or via pane.graphics.stream (raw RGBA frames).
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"log"
	"net"
	"os"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

type rpcReq struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type placement struct {
	ViewportCol int `json:"viewport_col"`
	ViewportRow int `json:"viewport_row"`
	GridCols    int `json:"grid_cols"`
	GridRows    int `json:"grid_rows"`
}

type frameHeader struct {
	Format      string    `json:"format"`
	ImageWidth  int       `json:"image_width"`
	ImageHeight int       `json:"image_height"`
	DataLength  int       `json:"data_length"`
	Sequence    uint64    `json:"sequence"`
	Revision    uint64    `json:"revision"`
	Placement   placement `json:"placement"`
}

func sockPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	return os.ExpandEnv("$HOME/.config/herdr/herdr.sock")
}

// call opens a fresh connection, sends exactly one request, reads one response line.
// herdr's socket server handles exactly one request per connection
// (src/api/server.rs:169 read_initial_request_line, no loop), so every call needs its own dial.
func call(method string, params interface{}) (map[string]interface{}, string, error) {
	c, err := net.Dial("unix", sockPath())
	if err != nil {
		return nil, "", err
	}
	defer c.Close()
	req := rpcReq{ID: "go_" + method, Method: method, Params: params}
	line, _ := json.Marshal(req)
	if _, err := c.Write(append(line, '\n')); err != nil {
		return nil, "", err
	}
	br := bufio.NewReader(c)
	resp, err := br.ReadString('\n')
	if err != nil && resp == "" {
		return nil, "", err
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &m); err != nil {
		return nil, resp, err
	}
	return m, resp, nil
}

// tightRGBA copies any image into a densely packed w*h*4 straight-RGBA buffer.
// go-pdfium hands back an *image.RGBA whose Stride is not guaranteed to equal 4*w,
// and herdr's stream reader expects exactly data_length = w*h*4 bytes with no row padding.
func tightRGBA(img image.Image) (int, int, []byte) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]byte, w*h*4)
	if src, ok := img.(*image.RGBA); ok {
		for y := 0; y < h; y++ {
			copy(out[y*w*4:(y+1)*w*4], src.Pix[(y+b.Min.Y-src.Rect.Min.Y)*src.Stride+(b.Min.X-src.Rect.Min.X)*4:])
		}
		return w, h, out
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	copy(out, dst.Pix)
	return w, h, out
}

func main() {
	paneID := flag.String("pane", "", "target pane id")
	pdfPath := flag.String("pdf", "", "pdf file")
	mode := flag.String("mode", "stream", "stream|set")
	dpi := flag.Int("dpi", 0, "render dpi (0 = fit pane)")
	page := flag.Int("page", 0, "0-based page index")
	frames := flag.Int("frames", 3, "stream mode: how many frames to send")
	flag.Parse()
	if *paneID == "" || *pdfPath == "" {
		log.Fatal("need --pane and --pdf")
	}

	// 1. geometry
	info, infoRaw, err := call("pane.graphics.info", map[string]string{"pane_id": *paneID})
	if err != nil {
		log.Fatalf("graphics.info: %v", err)
	}
	fmt.Println("INFO  >", infoRaw)
	res, _ := info["result"].(map[string]interface{})
	if res == nil {
		log.Fatal("graphics.info returned no result")
	}
	cellW := int(res["cell_width_px"].(float64))
	cellH := int(res["cell_height_px"].(float64))

	lay, layRaw, err := call("pane.layout", map[string]string{"pane_id": *paneID})
	if err != nil {
		log.Fatalf("pane.layout: %v", err)
	}
	fmt.Println("LAYOUT>", layRaw)
	cols, rows := 80, 24
	if lr, ok := lay["result"].(map[string]interface{}); ok {
		if l, ok := lr["layout"].(map[string]interface{}); ok {
			for _, p := range l["panes"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["pane_id"] == *paneID {
					r := pm["rect"].(map[string]interface{})
					cols = int(r["width"].(float64))
					rows = int(r["height"].(float64))
				}
			}
		}
	}
	targetW, targetH := cols*cellW, rows*cellH
	fmt.Printf("GEOM  > cell=%dx%dpx pane=%dx%d cells canvas=%dx%dpx\n", cellW, cellH, cols, rows, targetW, targetH)

	// 2. pdfium
	tInit := time.Now()
	cacheDir := "./wazero-cache"
	os.MkdirAll(cacheDir, 0o755)
	cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		log.Fatal(err)
	}
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle: 1, MaxIdle: 1, MaxTotal: 1,
		RuntimeConfig: wazero.NewRuntimeConfig().WithCompilationCache(cache),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	inst, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer inst.Close()
	initMS := ms(time.Since(tInit))
	fmt.Printf("PDFIUM> init_ms=%.1f\n", initMS)

	pdfBytes, err := os.ReadFile(*pdfPath)
	if err != nil {
		log.Fatal(err)
	}
	tOpen := time.Now()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &pdfBytes})
	if err != nil {
		log.Fatal(err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	fmt.Printf("PDFIUM> open_ms=%.1f bytes=%d\n", ms(time.Since(tOpen)), len(pdfBytes))

	pg := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: *page}}
	tRender := time.Now()
	var img image.Image
	if *dpi > 0 {
		r, err := inst.RenderPageInDPI(&requests.RenderPageInDPI{DPI: *dpi, Page: pg})
		if err != nil {
			log.Fatal(err)
		}
		img = r.Result.Image
	} else {
		r, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{Width: targetW, Height: targetH, Page: pg})
		if err != nil {
			log.Fatal(err)
		}
		img = r.Result.Image
	}
	renderMS := ms(time.Since(tRender))
	w, h, pix := tightRGBA(img)
	fmt.Printf("RENDER> render_ms=%.1f image=%dx%d rgba_bytes=%d\n", renderMS, w, h, len(pix))

	gridCols := (w + cellW - 1) / cellW
	gridRows := (h + cellH - 1) / cellH
	pl := placement{ViewportCol: 0, ViewportRow: 0, GridCols: gridCols, GridRows: gridRows}

	switch *mode {
	case "set":
		doSet(*paneID, img, pl)
	case "stream":
		doStream(*paneID, w, h, pix, pl, *frames)
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

func doSet(paneID string, img image.Image, pl placement) {
	tEnc := time.Now()
	var buf []byte
	{
		f, _ := os.CreateTemp("", "spike*.png")
		name := f.Name()
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(f, img); err != nil {
			log.Fatal(err)
		}
		f.Close()
		buf, _ = os.ReadFile(name)
		os.Remove(name)
	}
	encMS := ms(time.Since(tEnc))
	b64 := base64.StdEncoding.EncodeToString(buf)
	b := img.Bounds()
	fmt.Printf("SET   > png_encode_ms=%.1f png_bytes=%d base64_bytes=%d\n", encMS, len(buf), len(b64))

	tSend := time.Now()
	_, raw, err := call("pane.graphics.set", map[string]interface{}{
		"pane_id": paneID, "format": "png",
		"image_width": b.Dx(), "image_height": b.Dy(),
		"data_base64": b64, "z_index": 0, "placement": pl,
	})
	if err != nil {
		log.Fatalf("graphics.set: %v raw=%s", err, raw)
	}
	fmt.Printf("SET   > rtt_ms=%.1f resp=%s", ms(time.Since(tSend)), raw)
}

func doStream(paneID string, w, h int, pix []byte, pl placement, frames int) {
	c, err := net.Dial("unix", sockPath())
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	open, _ := json.Marshal(rpcReq{ID: "go_stream", Method: "pane.graphics.stream",
		Params: map[string]interface{}{"pane_id": paneID, "z_index": 0}})
	tOpen := time.Now()
	if _, err := c.Write(append(open, '\n')); err != nil {
		log.Fatal(err)
	}
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	openResp, err := br.ReadString('\n')
	if err != nil {
		log.Fatalf("stream open read: %v partial=%q", err, openResp)
	}
	fmt.Printf("OPEN  > rtt_ms=%.1f resp=%s", ms(time.Since(tOpen)), openResp)
	var om map[string]interface{}
	json.Unmarshal([]byte(openResp), &om)
	if _, bad := om["error"]; bad {
		log.Fatalf("stream open rejected: %s", openResp)
	}

	for i := 1; i <= frames; i++ {
		hdr, _ := json.Marshal(frameHeader{
			Format: "rgba", ImageWidth: w, ImageHeight: h, DataLength: len(pix),
			Sequence: uint64(i), Revision: uint64(i), Placement: pl,
		})
		fmt.Printf("FRAME%d> header=%s\n", i, string(hdr))
		c.SetWriteDeadline(time.Now().Add(15 * time.Second))
		tFrame := time.Now()
		if _, err := c.Write(append(hdr, '\n')); err != nil {
			log.Fatalf("frame header write: %v", err)
		}
		if _, err := c.Write(pix); err != nil {
			log.Fatalf("frame body write: %v", err)
		}
		sendMS := ms(time.Since(tFrame))

		// Inline frames are acked by silence: herdr writes a line back only when the
		// frame is rejected (src/api/server/pane_graphics_stream.rs:325-333).
		c.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
		line, rerr := br.ReadString('\n')
		if line != "" {
			fmt.Printf("FRAME%d> send_ms=%.1f SERVER REPLIED: %s", i, sendMS, line)
			log.Fatal("frame rejected, stream closed by server")
		}
		if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
			fmt.Printf("FRAME%d> send_ms=%.1f bytes=%d accepted (no reply within 1200ms = ok)\n", i, sendMS, len(pix))
			continue
		}
		log.Fatalf("frame %d: unexpected read error: %v", i, rerr)
	}

	// Hold the stream open so the layer stays on screen; the layer is cleared when
	// the connection closes (pane_graphics_stream.rs clear_layer on return).
	fmt.Println("HOLD  > stream open, sleeping 3600s (kill the process to clear the layer)")
	time.Sleep(3600 * time.Second)
}
