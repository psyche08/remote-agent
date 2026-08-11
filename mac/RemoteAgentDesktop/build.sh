#!/usr/bin/env bash
# Builds and signs agenthalo-desktop, the resident macOS helper.
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
#   AGENTHALO_SIGN_IDENTITY="Developer ID Application: ..." \
#     AGENTHALO_SIGN_TEAM_ID=ABCDE12345 ./build.sh
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

[ "$(uname -s)" = "Darwin" ] || { echo "agenthalo-desktop can only be built on macOS" >&2; exit 1; }
command -v swift >/dev/null 2>&1 || { echo "swift not found; install the Xcode command line tools" >&2; exit 127; }

echo "==> building agenthalo-desktop (release)"
( cd "$HERE" && swift build -c release --arch arm64 )
BINARY="$HERE/.build/arm64-apple-macosx/release/agenthalo-desktop"
[ -x "$BINARY" ] || BINARY="$HERE/.build/release/agenthalo-desktop"
[ -x "$BINARY" ] || { echo "build produced no binary" >&2; exit 1; }

# A standalone Developer ID executable cannot carry restricted Keychain access-
# group entitlements without an app-like wrapper and embedded provisioning
# profile. AgentHalo deliberately remains a bare Mach-O and uses the file-based
# login Keychain's creator-DR ACL, so this signature must contain no Keychain
# entitlement at all.
SIGNING_IDENTIFIER="dev.linsheng.agenthalo.desktop"
SIGN_TEAM_ID="${AGENTHALO_SIGN_TEAM_ID:-}"
if [ "$ADHOC" != "1" ]; then
  [ -n "$SIGN_TEAM_ID" ] || {
    echo "AGENTHALO_SIGN_TEAM_ID is required for a release signature" >&2
    exit 2
  }
fi

echo "==> signing"
if [ "$ADHOC" = "1" ]; then
  # An ad-hoc signature is acceptable only on a development machine you own.
  # It gives the helper an identity that does not survive a rebuild, so TCC
  # will re-prompt and previously granted permissions will not apply. Do not
  # attach a Keychain access-group entitlement: a bare Mach-O cannot satisfy it
  # without a provisioning profile and macOS kills it before main() runs.
  echo "    WARNING: ad-hoc signature — development only, TCC grants will not persist"
  codesign --force --identifier "$SIGNING_IDENTIFIER" --sign - --timestamp=none "$BINARY"
else
  IDENTITY="${AGENTHALO_SIGN_IDENTITY:-}"
  [ -n "$IDENTITY" ] || {
    echo "AGENTHALO_SIGN_IDENTITY is required (or pass --adhoc)" >&2
    exit 2
  }
  # The hardened runtime is what lets this binary hold TCC grants under a
  # stable identity across updates.
  codesign --force --identifier "$SIGNING_IDENTIFIER" --options runtime --timestamp \
    --sign "$IDENTITY" "$BINARY"
fi
codesign --verify --strict --verbose=2 "$BINARY"
SIGNED_ENTITLEMENTS="$(codesign -d --entitlements - "$BINARY" 2>&1 || true)"
if grep -qE 'keychain-access-groups|application-identifier|com\.apple\.developer\.team-identifier' \
     <<<"$SIGNED_ENTITLEMENTS"; then
  echo "signed helper unexpectedly contains a provisioning-profile Keychain entitlement" >&2
  exit 1
fi
ACTUAL_IDENTIFIER="$(codesign -d --verbose=4 "$BINARY" 2>&1 | sed -n 's/^Identifier=//p' | head -1)"
[ "$ACTUAL_IDENTIFIER" = "$SIGNING_IDENTIFIER" ] || {
  echo "signed helper Identifier ${ACTUAL_IDENTIFIER:-missing} does not match $SIGNING_IDENTIFIER" >&2
  exit 1
}
if [ "$ADHOC" != "1" ]; then
  ACTUAL_TEAM="$(codesign -d --verbose=4 "$BINARY" 2>&1 | sed -n 's/^TeamIdentifier=//p' | head -1)"
  [ "$ACTUAL_TEAM" = "$SIGN_TEAM_ID" ] || {
    echo "signed helper TeamIdentifier ${ACTUAL_TEAM:-missing} does not match AGENTHALO_SIGN_TEAM_ID=$SIGN_TEAM_ID" >&2
    exit 1
  }
fi

if [ -n "$OUT" ]; then
  mkdir -p "$(dirname "$OUT")"
  # Write beside the target and rename over it, never into it.
  #
  # Copying onto the path of a *running* helper writes through to the same
  # inode, which corrupts the image the kernel already mapped: macOS kills the
  # process with OS_REASON_CODESIGNING, and launchd respawns it into the same
  # broken file. Rename swaps in a new inode instead, so the running process
  # keeps the old one until it exits.
  #
  # The signature travels either way — it lives inside the Mach-O, not beside
  # it — so this is about the file, not the signing.
  TMP_OUT="$(dirname "$OUT")/.$(basename "$OUT").new.$$"
  cp "$BINARY" "$TMP_OUT"
  chmod 0755 "$TMP_OUT"
  mv -f "$TMP_OUT" "$OUT"
  echo "==> wrote $OUT"
else
  echo "==> built $BINARY"
fi
