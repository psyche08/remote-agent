#!/usr/bin/env bash
# Builds and signs the Locked Use Authorization Plug-in bundle.
#
# Must run ON the target Mac (or a Mac with the same toolchain): the bundle is
# loaded into the SecurityAgent's process, so it has to be built and signed
# locally. CI does not build this.
#
# Signing is not optional. macOS loads authorization plug-ins from a
# root-owned directory, and an unsigned bundle there is exactly the kind of
# thing this feature must not normalize. Set AGENTHALO_PLUGIN_SIGN_IDENTITY to a
# Developer ID Application identity.
#
# Usage:
#   AGENTHALO_PLUGIN_SIGN_IDENTITY="Developer ID Application: ..." ./build.sh
#   ./build.sh --adhoc      # local development only; see the warning below
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${AGENTHALO_PLUGIN_BUILD_DIR:-$HERE/build}"
BUNDLE_NAME="AgentHaloLockedUse.bundle"
BUNDLE="$BUILD_DIR/$BUNDLE_NAME"
SIGNING_IDENTIFIER="dev.linsheng.agenthalo.locked-use.plugin"
ADHOC=0

for arg in "$@"; do
  case "$arg" in
    --adhoc) ADHOC=1 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

if [ "$(uname -s)" != "Darwin" ]; then
  echo "the Locked Use authorization plug-in can only be built on macOS" >&2
  exit 1
fi
command -v clang >/dev/null 2>&1 || { echo "clang not found; install Xcode command line tools" >&2; exit 127; }

echo "==> building $BUNDLE_NAME"
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/Contents/MacOS"
cp "$HERE/Info.plist" "$BUNDLE/Contents/Info.plist"

clang -bundle \
  -fobjc-arc \
  -mmacosx-version-min=12.0 \
  -arch arm64 -arch x86_64 \
  -framework Foundation \
  -framework Security \
  -o "$BUNDLE/Contents/MacOS/AgentHaloLockedUse" \
  "$HERE/AgentHaloLockedUse.m"

echo "==> signing"
if [ "$ADHOC" = "1" ]; then
  # An ad-hoc signature is acceptable only on a development machine you own.
  # It provides no authorship guarantee: anything that can write the plug-in
  # directory could replace this bundle with another ad-hoc one.
  echo "    WARNING: ad-hoc signature — development only, do not deploy"
  codesign --force --identifier "$SIGNING_IDENTIFIER" --sign - --timestamp=none "$BUNDLE"
else
  IDENTITY="${AGENTHALO_PLUGIN_SIGN_IDENTITY:-}"
  if [ -z "$IDENTITY" ]; then
    echo "AGENTHALO_PLUGIN_SIGN_IDENTITY is required (or pass --adhoc for local dev)" >&2
    exit 2
  fi
  codesign --force --identifier "$SIGNING_IDENTIFIER" --options runtime --timestamp \
    --sign "$IDENTITY" "$BUNDLE"
fi

codesign --verify --strict --verbose=2 "$BUNDLE"
echo "==> built $BUNDLE"
echo "    install with: sudo ./install.sh"
