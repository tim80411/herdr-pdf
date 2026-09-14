# Spike: draw a PNG into a herdr pane via `pane.graphics.set`

Feasibility check for the herdr PDF viewer plugin: can we push a rendered
page image into a pane over the socket API, and does it actually paint?

## TL;DR

The socket call chain works end-to-end (`ok` acks, no server-side errors or
drop warnings in the log), but **visual confirmation failed** because
`screencapture` could not access the display in this environment
(`could not create image from display`, 3/3 attempts). So: **API-level
success confirmed, pixel-level success unconfirmed.**

## How to run it

```bash
# draw a PNG into a disposable pane (NEVER your own active pane)
python3 draw_png.py --pane w8:p2 --png testpage-1.png

# clear it again
python3 draw_png.py --pane w8:p2 --clear
```

Pure stdlib, no dependencies. Reads `HERDR_SOCKET_PATH` from the environment
(falls back to `~/.config/herdr/herdr.sock`). See the module docstring in
`draw_png.py` for the full protocol notes.

## What the spike did

1. `pane.graphics.info` on the current pane (`w8:p1`, read-only, never
   drawn on) — confirmed graphics capability, cell size `8x19` px.
2. Converted page 1 of a local PDF to PNG with `pdftoppm -r 80` →
   `662x936` px, 149 KB (well under the 512 KiB inline cap).
3. `herdr pane split w8:p1 --direction right` → new pane `w8:p2`
   (106 cols x 61 rows).
4. `pane.graphics.info` on `w8:p2` → same cell size, `pane_visible: false`.
5. `pane.graphics.set` with `placement {viewport_col:0, viewport_row:0,
   grid_cols:83, grid_rows:50}` → `{"result":{"type":"ok"}}` in 155.6 ms.
6. Verification:
   - Server log (`~/.config/herdr/herdr-server.log`) showed
     `api.request.start` / `api.request.complete outcome="ok"` for
     `pane.graphics.set`, and **no** `dropping graphics assets from
     oversized pane surface` warning.
   - `pane.graphics.info` called again afterward: still no per-layer
     info in the response (the schema doesn't expose active layers at
     all), and `pane_visible` was still `false`.
   - `screencapture -x` failed 3/3 times: `could not create image from
     display` (exit 1). Confirmed not a stray-file issue (no partial PNGs
     were written). Likely cause: this session's terminal process lacks
     macOS Screen Recording (TCC) permission, or the physical display
     session isn't reachable from this process — consistent with
     `pane_visible: false` even before any drawing was attempted, and with
     `herdr pane list` showing workspace `w9` (not `w8`) as the focused
     workspace at the time.
7. Cleanup: `pane.graphics.clear` on `w8:p2` (via `draw_png.py --clear`,
   `ok`), then `herdr pane close w8:p2`. Confirmed gone from
   `herdr pane list`. `w8:p1` was never touched by any graphics call.

## Measured numbers

| Metric | Value |
| --- | --- |
| PNG size | 149 KB (152,947 bytes) |
| PNG dimensions | 662 x 936 px |
| Target pane size | 106 cols x 61 rows |
| Cell size (1st query) | 8 x 19 px |
| Cell size (2nd query, after set+clear) | 15 x 38 px — **changed between calls** |
| Placement grid used | grid_cols=83, grid_rows=50 at (0,0) |
| `pane.graphics.set` round-trip | 155.6 ms |
| `pane.graphics.info` round-trip | 0.2-0.6 ms |
| `screencapture` attempts | 3/3 failed, "could not create image from display" |

## Implications for the PDF viewer plugin design

1. **API success != visual success.** `pane.graphics.set` acking `ok` only
   proves the request was well-formed and accepted — it does not prove a
   frame is on screen. Given the wire-level 32 MiB aggregate frame limit
   silently drops oversized frames (per docs), the plugin needs its own
   correctness signal beyond the ack — e.g. watching the server log for
   `dropping graphics assets`, or, once available, an actual visible-pixel
   check. Relying on the ack alone would let silent render failures ship.
2. **Cell size is not stable and must be re-queried, not cached.** The same
   pane reported `8x19` px cells on the first `pane.graphics.info` call and
   `15x38` px on a later call (same session, no explicit pane resize
   observed by this script). A PDF viewer plugin must call
   `pane.graphics.info` immediately before every `pane.graphics.set` to
   recompute `grid_cols`/`grid_rows`, not memoize it at startup — otherwise
   page images will be mis-scaled or clipped after a font/DPI/window change.
3. **`pane_visible` and screen capture are two different visibility
   signals, and both were unavailable here.** `pane_visible` reflects
   workspace/tab/zoom focus server-side; it was `false` throughout because
   a different workspace (`w9`) was focused. `screencapture` failing
   separately (TCC/display access) means even a `pane_visible: true` pane
   can't be screenshotted from this kind of process. The plugin's own test
   suite should not depend on OS-level screenshotting for CI; it should
   assert on `pane_visible` plus server-log absence of drop warnings as the
   practical proxy, and treat true pixel verification as a manual/local-only
   check.

## Left undone / not cleaned up

- **Pixel-level visual confirmation was not obtained.** `screencapture`
  failed 3/3 times in this environment; nobody has visually confirmed the
  PNG actually appeared on screen. This is the single open question before
  calling the spike a full pass — needs a re-run from a session where
  `screencapture` has Screen Recording permission (or `w8` is the focused
  workspace on a reachable display), or an alternative verification path
  (e.g. a herdr-side pixel/layer introspection API, if one exists, wasn't
  explored here).
- `pane.graphics.info`'s response schema was read only from the two live
  calls made in this spike, not cross-checked against
  `src/api/schema/panes.rs` for a complete field list (e.g. whether a
  layer-listing field exists under a different name). Worth a quick
  grep before concluding the API truly has no layer-introspection field.
- No `git commit` made, per the task boundary. `spike/draw_png.py` and
  `spike/README.md` are new untracked files in
  `/Users/tim80411/self/products/herdr-pdf` (which is already a git repo).
- The source test PNG (`testpage-1.png`, from a personal PDF in
  `~/Downloads`) and this session's `herdr_call.py` /
  `do_graphics_set.py` scratch helpers were left in the scratchpad
  directory (`.../scratchpad/herdr-pdf-research/`), not copied into
  `spike/` — only the stdlib-only combined `draw_png.py` was placed there,
  per the task's file-placement instructions. Generate a fresh test PNG
  locally when re-running (see `draw_png.py`'s docstring for the
  `pdftoppm` command used here).
