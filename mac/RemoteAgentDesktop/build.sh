#!/usr/bin/env bash
# Builds and signs remote-agent-desktop, the resident macOS helper.
#
# Must run on a Mac: the helper is Swift against AppKit and CoreGraphics.
#
# Signing is not decoration here. The helper holds the Accessibility and Screen
# Recording grants, and TCC binds those to a code signature — an unsigned or
# ad-hoc build gets a *different* identity, so its grants do not carry over and
# every synthetic event silently does nothing. Release builds must be signed
# with the same Developer ID the agent binary uses.
#
# Usage:
#   RA_SIGN_IDENTITY="Developer ID Application: ..." ./build.sh
#   ./build.sh --adhoc          # local development only
#   ./build.sh --out <path>     # copy the signed binary somewhere
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ADHOC=0
OUT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --adhoc) ADHOC=1 ;;
    --out) shift; OUT="${1:-}"; [ -n "$OUT" ] || { echo "--out requires a path" >&2; exit 2; } ;;
    -h|--help) sed -n '2,18p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(uname -s)" = "Darwin" ] || { echo "remote-agent-desktop can only be built on macOS" >&2; exit 1; }
command -v swift >/dev/null 2>&1 || { echo "swift not found; install the Xcode command line tools" >&2; exit 127; }

echo "==> building remote-agent-desktop (release)"
( cd "$HERE" && swift build -c release --arch arm64 )
BINARY="$HERE/.build/arm64-apple-macosx/release/remote-agent-desktop"
[ -x "$BINARY" ] || BINARY="$HERE/.build/release/remote-agent-desktop"
[ -x "$BINARY" ] || { echo "build produced no binary" >&2; exit 1; }

echo "==> signing"
if [ "$ADHOC" = "1" ]; then
  # An ad-hoc signature is acceptable only on a development machine you own.
  # It gives the helper an identity that does not survive a rebuild, so TCC
  # will re-prompt and previously granted permissions will not apply.
  echo "    WARNING: ad-hoc signature — development only, TCC grants will not persist"
  codesign --force --sign - --timestamp=none "$BINARY"
else
  IDENTITY="${RA_SIGN_IDENTITY:-}"
  [ -n "$IDENTITY" ] || { echo "RA_SIGN_IDENTITY is required (or pass --adhoc)" >&2; exit 2; }
  # The hardened runtime is what lets this binary hold TCC grants under a
  # stable identity across updates.
  codesign --force --options runtime --timestamp --sign "$IDENTITY" "$BINARY"
fi
codesign --verify --strict --verbose=2 "$BINARY"

if [ -n "$OUT" ]; then
  mkdir -p "$(dirname "$OUT")"
  # cp preserves the signature: it lives inside the Mach-O, not beside it.
  cp "$BINARY" "$OUT"
  echo "==> wrote $OUT"
else
  echo "==> built $BINARY"
fi
