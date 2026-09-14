# herdr-pdf

A herdr plugin that renders PDF pages as images in a split pane, so you can
read a PDF without leaving your terminal workspace.

This is a single Go binary (`bin/herdr-pdf`) — no Python, no venv, no
system PDF library. The original Python MVP was removed once the Go version reached parity; `spike/` keeps the two feasibility spikes (Python `pane.graphics.set`, Go `pane.graphics.stream`).

## Install

Not yet published. For now, link it as a local plugin:

```sh
herdr plugin link /path/to/herdr-pdf
herdr plugin enable pdf   # if it doesn't come up enabled
```

Linking (or updating) runs `./build.sh`, which just needs a Go toolchain
(`go` on `$PATH`, or `$GO` pointing at one) and compiles `bin/herdr-pdf`.
Once published: `herdr plugin install <owner>/herdr-pdf`.

## Usage

- **Ctrl+click** a path or `file://` link ending in `.pdf` in any herdr pane.
- Or invoke the action directly:
  `herdr plugin action invoke pdf.open -- /path/to/file.pdf`
- Or run the opener binary by hand: `./bin/herdr-pdf open /path/to/file.pdf`

Any of these opens the viewer in a new split pane to the right of the
current one.

## Keys

| Key | Action |
| --- | --- |
| `j`, `↓`, `space`, `n`, `PgDn` | Next page |
| `k`, `↑`, `p`, `PgUp` | Previous page |
| `g` | First page |
| `G` | Last page (or jump to page N if you typed digits first) |
| `0`-`9` then `G` or Enter | Jump to page N |
| `r` | Redraw (e.g. after a display glitch) |
| `q`, `Ctrl-C` | Quit |

## How it works

The viewer takes over its pane's terminal (alternate screen, raw mode,
hidden cursor) and rasterizes each page with
[go-pdfium](https://github.com/klippa-app/go-pdfium) (PDFium compiled to
WebAssembly, run via [wazero](https://wazero.io) — no cgo, no system
Poppler/MuPDF). The bottom row is kept as a plain-text status line; the
page image fills the rows above it, scaled to fit the pane's current pixel
size and centered.

Pixels reach the pane over herdr's `pane.graphics.stream` socket API: a
long-lived connection opened once and held for the life of the pane (the
layer is cleared automatically when the connection closes). Frames are sent
as **PNG**, not raw RGBA, and that choice is the whole reason page flips
feel instant.

herdr applies no compression of its own anywhere between the plugin and the
terminal — no zlib, no `o=z`. Whatever the plugin hands over is stored as an
owned copy, hashed whole, sent to the client in an uncompressed bincode
message, then base64-expanded 4:3 and cut into 3072-byte Kitty chunks. A
fit-to-pane RGBA frame is ~3.7 MB, which becomes ~4.9 MB of escape text in
~1,200 chunks for the terminal to parse and decode on *every* keypress. The
same page as PNG is ~330 KB. herdr never decodes PNG: it hands Kitty format
code 100 straight through, so compressing the frame shrinks every stage
downstream rather than only the socket write.

### Remote sessions

Over SSH the picture gets worse, because nothing on the path compresses:
herdr has no compression crate at all, it invokes `ssh` without `-C`, and
the graphics payload is serialised into the *same* bincode message as the
pane's text surface (`protocol/wire.rs:1213-1222`) — so a fat page delays
the text riding with it. The server→client render channel then holds
exactly one message and **drops rather than queues** when the link is busy
(`server/client_transport.rs:119-124`, "Capacity is one so slow clients
cannot build lag"), so a frame that arrives mid-transfer is discarded and
retried on a later tick. That retry is the lag a reader feels.

The viewer detects this from `pane.graphics.info`: `file_frame_transport` is
only ever `direct-kitty` for a terminal on the same machine, so an empty
value means remote (or nothing attached). Two behaviours switch on:

- **A frame budget** (200 KB remote, 1500 KB local). Pages that exceed it
  step down a ladder — truecolour, then 8-bit greyscale, then 16-level
  greyscale (which PNG stores at 4 bits per pixel), then progressive
  downscaling. Colour depth goes first because it is nearly free on black
  ink and white paper; resolution goes last because it is the only rung
  that costs legibility.
- **Progressive refinement.** The keypress sends a ~10-20 KB preview that
  lands in one tick, and the full page follows about 90 ms later. The delay
  has to exceed herdr's 16 ms render cadence (`app/mod.rs:38`) or the
  server coalesces both into one tick and the preview is never drawn. A
  second keypress inside that window cancels the pending refinement, so
  holding `j` down sends previews only — exactly when latency matters and
  sharpness does not.

A scaled-down encode still fills the pane rather than shrinking: herdr
writes the image's own pixels as `s=`/`v=` but takes the cell box from the
placement as `c=`/`r=` (`kitty_graphics.rs:799`), so the terminal scales it
back up. A preview looks soft, not small.

### Environment variables

| Variable | Values | Effect |
| --- | --- | --- |
| `HERDR_PDF_TRANSPORT` | `stream-png` (default), `stream-rgba`, `direct`, `set` | Which path frames take to the terminal. |
| `HERDR_PDF_MAX_FRAME_KB` | positive integer | Override the frame budget. |
| `HERDR_PDF_PROGRESSIVE` | `1` / `0` | Force progressive refinement on or off instead of deciding from the session. |
| `HERDR_PDF_TRACE` | `1` / `true` / `yes` / `on` | Log a per-stage timing line for every redraw to `viewer.log`. |

`stream-rgba` is the old raw-RGBA behaviour. `direct` writes RGBA to a file
in `pane.graphics.info`'s `file_frame_directory` and sends only the path,
which lets herdr use Kitty's `t=f` transport with no base64 at all — but
herdr gates that on the layer being literally `primary` *and* the client
having negotiated `direct-kitty`, so this mode gives up the named layer and
falls back to `stream-png` when the handshake did not enable it. `set` is
the old Python viewer's path: one short-lived `pane.graphics.set` call per
page, capped at 512 KiB decoded, with a shrink loop for pages that don't
fit. `set` is also the automatic fallback whenever the stream cannot be
opened or fails partway through a session.

All four are forwarded into the pane by `herdr-pdf open`, so
`HERDR_PDF_TRACE=1 herdr-pdf open file.pdf` works from a shell.

Two caches keep work off the keypress path. The page either side of the
current one is rendered *and* PNG-encoded on a background goroutine right
after a page is drawn, so a normal forward or backward flip finds both ready
and costs about a millisecond instead of the 25-70 ms a render takes. Cell
pixel size (`pane.graphics.info`) is cached rather than re-read per flip: it
is not guaranteed to be stable across clients, so it is refreshed on resize
and re-read in the background when it ages out, but a keypress only ever
blocks on it when there is no usable value at all.

## Building from source

```sh
go build -trimpath -ldflags="-s -w" -o bin/herdr-pdf ./cmd/herdr-pdf
# or just:
./build.sh
```

Requires Go 1.24+. `go vet ./...` and `go test ./...` should both pass
clean.

## Architecture

```
cmd/herdr-pdf/        CLI entry point: view / open / version subcommands
internal/herdr/       socket client (short-lived Call) + long-lived StreamSession
internal/render/      go-pdfium/wazero page rasterization + small page LRU cache
internal/ui/          raw-mode terminal lifecycle, key decoding, status-line layout
internal/viewer/      the pane's state machine: key handling, resize, render+send
internal/openpath/    path resolution for the "open" action / link handler
```

## Known limitations

- **Requires a Kitty-graphics-capable (KGP) herdr client.** Panes without
  graphics support will accept the API calls but show nothing; the status
  line will say so (`pane.graphics.info` reporting `cell_size_unavailable`
  or `feature_disabled`).
- **No popup support.** herdr's `popup` placement deliberately strips
  `HERDR_PANE_ID` from the launched process, so a popup pane cannot call
  `pane.graphics.*` at all. This plugin only ever opens `split` panes.
- **No text search or zoom yet.** The page is always fit to the pane at
  its current size; there is no independent zoom/pan.
- Only tested on macOS against herdr 0.9.0.

## License

MIT. PDFium (via go-pdfium/wazero) is BSD-3-Clause.
