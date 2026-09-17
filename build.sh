#!/bin/sh
# herdr [[build]] step for herdr-pdf.
#
# Fast path: download the prebuilt binary matching this source's declared
# version + platform from the GitHub release, verify its SHA-256, and
# install it at bin/herdr-pdf. Fallback: on any miss (no asset for this
# version, network error, checksum mismatch, unsupported platform, no
# curl/wget, no sha256 tool) build from source with Go instead — identical
# to the pre-prebuilt behavior, so installing never gets harder than
# before.
#
# Env overrides:
#   HERDR_PDF_RELEASE_BASE       override the release download base URL
#   HERDR_PDF_FORCE_BUILD=1      skip the download, always build from source
#   HERDR_PDF_SKIP_BUILD_FALLBACK=1   fail instead of building from source on a download miss
#   GO                           path to the go binary (default: "go")

set -eu

cd "$(dirname "$0")"

repo="tim80411/herdr-pdf"
base_url="${HERDR_PDF_RELEASE_BASE:-https://github.com/$repo/releases/download}"
manifest="herdr-plugin.toml"
out="bin/herdr-pdf"

have() { command -v "$1" >/dev/null 2>&1; }

download() { # download <url> <dest>
  if have curl; then
    curl -fsSL -o "$2" "$1"
  elif have wget; then
    wget -q -O "$2" "$1"
  else
    return 127
  fi
}

sha256_of() { # prints the hex digest of file $1
  if have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  else
    return 127
  fi
}

# version_ge A B -> true (exit 0) if dotted numeric version A >= B
version_ge() {
  awk -v a="$1" -v b="$2" 'BEGIN {
    n = split(a, A, ".");
    m = split(b, B, ".");
    max = (n > m) ? n : m;
    for (i = 1; i <= max; i++) {
      x = (i <= n) ? A[i] : 0;
      y = (i <= m) ? B[i] : 0;
      if (x + 0 > y + 0) exit 0;
      if (x + 0 < y + 0) exit 1;
    }
    exit 0
  }'
}

version=$(grep -E '^version *= *"' "$manifest" 2>/dev/null | head -n 1 | sed -E 's/^version *= *"([^"]+)".*/\1/')
if [ -z "${version:-}" ]; then
  echo "herdr-pdf build: could not read version from $manifest" >&2
  exit 1
fi

go_required=$(grep -E '^go +[0-9]' go.mod 2>/dev/null | awk '{print $2}')

# check_go: sets $GO to a usable Go binary meeting go_required, or fails.
check_go() {
  GO="${GO:-go}"
  have "$GO" || return 1
  gv=$("$GO" version 2>/dev/null | sed -E 's/^go version go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/')
  [ -n "$gv" ] || return 1
  version_ge "$gv" "$go_required"
}

build_from_source() {
  if ! check_go; then
    if have "${GO:-go}"; then
      echo "herdr-pdf build: found $("${GO:-go}" version 2>/dev/null) but need go >= $go_required (see go.mod). Install a newer Go from https://go.dev/dl/, or set \$GO to point at one, then re-run." >&2
    else
      echo "herdr-pdf build: no Go toolchain found (looked for '${GO:-go}' in PATH) and no prebuilt binary is available for v$version yet. Install Go >= $go_required from https://go.dev/dl/ (or set \$GO to its path), or wait for a v$version release to publish prebuilt binaries, then re-run." >&2
    fi
    exit 1
  fi
  mkdir -p bin
  "$GO" build -trimpath -ldflags="-s -w -X main.version=$version" -o "$out" ./cmd/herdr-pdf
  "$out" version
}

fallback() { # fallback <reason>
  echo "herdr-pdf build: $1 — falling back to go build." >&2
  if [ "${HERDR_PDF_SKIP_BUILD_FALLBACK:-}" = "1" ]; then
    echo "herdr-pdf build: HERDR_PDF_SKIP_BUILD_FALLBACK=1, not building from source." >&2
    exit 1
  fi
  build_from_source
  exit 0
}

if [ "${HERDR_PDF_FORCE_BUILD:-}" = "1" ]; then
  echo "herdr-pdf build: HERDR_PDF_FORCE_BUILD=1, skipping prebuilt download." >&2
  build_from_source
  exit 0
fi

# Idempotent: a binary already installed at the declared version is left alone.
if [ -x "$out" ] && "$out" version 2>/dev/null | grep -q " $version "; then
  echo "herdr-pdf build: $out already at v$version, skipping."
  exit 0
fi

# --- resolve the target asset from the platform --------------------------
os_raw=$(uname -s 2>/dev/null || echo unknown)
arch_raw=$(uname -m 2>/dev/null || echo unknown)

os=""
case "$os_raw" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  MINGW*|MSYS*|CYGWIN*) os=windows ;;
esac

arch=""
case "$arch_raw" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
esac

if [ -z "$os" ] || [ -z "$arch" ]; then
  fallback "no prebuilt binary for $os_raw/$arch_raw"
fi

ext=""
[ "$os" = "windows" ] && ext=".exe"
asset="herdr-pdf_${version}_${os}_${arch}${ext}"

tmpdir=$(mktemp -d 2>/dev/null) || fallback "could not create a temp dir"
trap 'rm -rf "$tmpdir"' EXIT

bin_url="$base_url/v$version/$asset"
sums_url="$base_url/v$version/checksums.txt"
tmpbin="$tmpdir/$asset"
tmpsums="$tmpdir/checksums.txt"

download "$bin_url" "$tmpbin" || fallback "prebuilt binary not available for v$version ($asset)"
download "$sums_url" "$tmpsums" || fallback "checksums.txt not available for v$version"

expected=$(grep -E "^[0-9a-f]{64}  ?\*?$asset\$" "$tmpsums" 2>/dev/null | awk '{print $1}' | head -n 1)
[ -n "$expected" ] || fallback "no checksum listed for $asset in checksums.txt"

actual=$(sha256_of "$tmpbin") || fallback "no sha-256 tool (shasum/sha256sum) available"
if [ "$actual" != "$expected" ]; then
  fallback "checksum mismatch for $asset (expected $expected, got $actual)"
fi

chmod +x "$tmpbin"
mkdir -p bin
mv -f "$tmpbin" "$out"
echo "herdr-pdf build: installed prebuilt v$version ($os/$arch), verified SHA-256."
"$out" version
