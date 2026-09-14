#!/bin/sh
# Build step for the herdr PDF Viewer plugin (Go rewrite).
#
# Produces a single binary at bin/herdr-pdf with no runtime dependency
# beyond the OS: PDFium ships embedded as WebAssembly via go-pdfium/wazero,
# so there is no system Poppler/MuPDF, no venv, and no dependency-version
# drift to pin (the whole point of the P0 fix in ARCH-review's Roadmap that
# said the Python build.sh's unpinned `pip install` was a future breakage
# waiting to happen).
#
# This is intentionally the simple "just build from source" step for now —
# ARCH-review's Roadmap suggested a prebuilt-binary-first install (like
# herdr-file-viewer's fetch-or-build.sh) as a later improvement once the Go
# rewrite has settled.
set -eu

cd "$(dirname "$0")"

GO="${GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
  echo "herdr-pdf build: no Go toolchain found (looked for '$GO' in PATH)." >&2
  echo "Install Go (https://go.dev/dl/) or set \$GO to its path, then re-run." >&2
  exit 1
fi

mkdir -p bin
"$GO" build -trimpath -ldflags="-s -w" -o bin/herdr-pdf ./cmd/herdr-pdf

./bin/herdr-pdf version
