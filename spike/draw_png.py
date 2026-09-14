#!/usr/bin/env python3
"""Draw a PNG into a herdr pane over the socket API (pure stdlib, no deps).

Spike script for the herdr PDF viewer plugin feasibility check. Proves that
`pane.graphics.set` over the herdr socket API actually paints an image into a
pane (not just that the request is ack'd).

Flow:
  1. pane.graphics.info  -> capability probe (cell_width_px/cell_height_px,
     pane_visible, feature/format limits). Aborts early on
     `cell_size_unavailable` / `feature_disabled`.
  2. Read the PNG's own IHDR chunk for width/height (no Pillow needed).
  3. Compute a `placement` grid (grid_cols/grid_rows) from image px size and
     the pane's cell px size, anchored at the given viewport col/row.
  4. pane.graphics.set    -> send the base64-encoded PNG.
  5. pane.graphics.info again, so you can eyeball `pane_visible` before/after.

Wire protocol: newline-delimited JSON over a Unix domain socket
(`$HERDR_SOCKET_PATH`, default `~/.config/herdr/herdr.sock`). One JSON object
per line in, one JSON object with the matching "id" back.
Docs: docs/next/website/src/content/docs/socket-api.mdx in the herdr repo,
section "Socket transport" and the `pane.graphics.*` method table.

IMPORTANT SAFETY NOTE: `pane.graphics.set` PAINTS OVER the target pane's
terminal content. Never point this at a pane you (or someone else) is
actively using unless you mean to overwrite what they see. Always pass a
pane_id for a disposable/scratch pane, e.g. one created with
`herdr pane split <pane_id> --direction right`.

Usage:
  python3 draw_png.py --pane w8:p2 --png testpage-1.png
  python3 draw_png.py --pane w8:p2 --png testpage-1.png --col 0 --row 0
  python3 draw_png.py --pane w8:p2 --clear     # clear the layer instead

Known limits observed in this spike (herdr v0.9.0):
  - Inline base64 PNG payload cap: 512 KiB (server returns `image_too_large`
    above that -- re-encode/downsample before sending).
  - Aggregate render-frame wire cap: 32 MiB per pane surface. Exceeding it is
    NOT reported back as an error to this request -- the server silently
    drops the oversized graphics asset and still acks `ok`. Cross-check the
    server log (`~/.config/herdr/herdr-server.log`, grep for "graphics",
    "oversized", "dropping") after any `set` call on a densely-layered pane.
  - `pane.graphics.info`'s response never lists currently-active layers, so
    API polling alone cannot confirm a frame rendered -- only that the pane
    *can* accept graphics. A visual check (screenshot) or a "does the pane's
    terminal buffer/scroll state change" proxy is needed for real proof.
"""
import argparse
import base64
import json
import os
import socket
import struct
import sys
import time

SOCK_PATH = os.environ.get(
    "HERDR_SOCKET_PATH", os.path.expanduser("~/.config/herdr/herdr.sock")
)


def call(req_obj, timeout=10.0):
    """Send one newline-delimited JSON request, return (response_obj, elapsed_ms)."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(SOCK_PATH)
    try:
        line = json.dumps(req_obj) + "\n"
        t0 = time.time()
        s.sendall(line.encode("utf-8"))
        buf = b""
        want_id = req_obj.get("id")
        while True:
            chunk = s.recv(65536)
            if not chunk:
                break
            buf += chunk
            while b"\n" in buf:
                raw, buf = buf.split(b"\n", 1)
                if not raw.strip():
                    continue
                obj = json.loads(raw.decode("utf-8"))
                if obj.get("id") == want_id:
                    return obj, (time.time() - t0) * 1000
    finally:
        s.close()
    return None, (time.time() - t0) * 1000


def png_dimensions(raw: bytes):
    if raw[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError("not a PNG file (bad signature)")
    # IHDR is always the first chunk: [4-byte length]['IHDR'][width u32be][height u32be]...
    width, height = struct.unpack(">II", raw[16:24])
    return width, height


def graphics_info(pane_id):
    req = {"id": "info", "method": "pane.graphics.info", "params": {"pane_id": pane_id}}
    resp, elapsed_ms = call(req)
    return resp, elapsed_ms


def graphics_clear(pane_id, layer_id=None):
    params = {"pane_id": pane_id}
    if layer_id:
        params["layer_id"] = layer_id
    req = {"id": "clear", "method": "pane.graphics.clear", "params": params}
    return call(req)


def graphics_set(pane_id, png_path, col, row, cell_w, cell_h, layer_id=None, z_index=None):
    with open(png_path, "rb") as f:
        raw = f.read()
    width, height = png_dimensions(raw)
    grid_cols = (width + cell_w - 1) // cell_w
    grid_rows = (height + cell_h - 1) // cell_h
    params = {
        "pane_id": pane_id,
        "format": "png",
        "image_width": width,
        "image_height": height,
        "data_base64": base64.b64encode(raw).decode("ascii"),
        "placement": {
            "viewport_col": col,
            "viewport_row": row,
            "grid_cols": grid_cols,
            "grid_rows": grid_rows,
        },
    }
    if layer_id:
        params["layer_id"] = layer_id
    if z_index is not None:
        params["z_index"] = z_index
    req = {"id": "set", "method": "pane.graphics.set", "params": params}
    resp, elapsed_ms = call(req)
    meta = {
        "png_bytes": len(raw),
        "width": width,
        "height": height,
        "grid_cols": grid_cols,
        "grid_rows": grid_rows,
    }
    return resp, elapsed_ms, meta


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--pane", required=True, help="target pane id, e.g. w8:p2 (NEVER your own active pane)")
    ap.add_argument("--png", help="path to a PNG file to draw")
    ap.add_argument("--col", type=int, default=0, help="viewport_col (default 0)")
    ap.add_argument("--row", type=int, default=0, help="viewport_row (default 0)")
    ap.add_argument("--layer", default=None, help="layer_id (default: primary)")
    ap.add_argument("--z-index", type=int, default=None)
    ap.add_argument("--clear", action="store_true", help="clear the layer instead of drawing")
    args = ap.parse_args()

    print(f"# socket={SOCK_PATH}")

    info_resp, ms = graphics_info(args.pane)
    print(f"# pane.graphics.info ({ms:.1f}ms):")
    print(json.dumps(info_resp, indent=2))

    if info_resp is None or "error" in info_resp:
        print("# aborting: pane.graphics.info failed", file=sys.stderr)
        sys.exit(1)

    result = info_resp.get("result", {})
    err_code = result.get("code") if isinstance(result, dict) else None
    if err_code in ("cell_size_unavailable", "feature_disabled"):
        print(f"# aborting: {err_code}", file=sys.stderr)
        sys.exit(1)

    if args.clear:
        resp, ms = graphics_clear(args.pane, args.layer)
        print(f"# pane.graphics.clear ({ms:.1f}ms):")
        print(json.dumps(resp, indent=2))
        return

    if not args.png:
        print("# nothing to do: pass --png <file> or --clear", file=sys.stderr)
        sys.exit(1)

    cell_w = result["cell_width_px"]
    cell_h = result["cell_height_px"]

    set_resp, ms, meta = graphics_set(
        args.pane, args.png, args.col, args.row, cell_w, cell_h, args.layer, args.z_index
    )
    print(f"# pane.graphics.set ({ms:.1f}ms): {meta}")
    print(json.dumps(set_resp, indent=2))

    info_resp2, ms2 = graphics_info(args.pane)
    print(f"# pane.graphics.info after set ({ms2:.1f}ms):")
    print(json.dumps(info_resp2, indent=2))


if __name__ == "__main__":
    main()
