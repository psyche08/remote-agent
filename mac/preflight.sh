#!/usr/bin/env bash
# Preflight for computer use + Locked Use on the target Mac.
#
# Everything here is what CI and a Linux container cannot check: the Swift
# helper and the Objective-C authorization plug-in only compile and run on
# macOS, and the two sides of the grant contract can only be run against each
# other here. Run this first on the machine you intend to use, before
# installing anything.
#
#   cd /path/to/remote-agent && bash mac/preflight.sh
#
# READ-ONLY BY DEFAULT. It never locks your screen, never raises the display
# shield, never installs the plug-in, and never edits the authorization
# database. The two checks that would disrupt a live desktop are opt-in:
#
#   --check-shield   raise the shield for ~2s, then drop it
#   --check-lock     lock the screen (you will need to log back in)
#
# Exit: 0 all attempted checks passed · 1 something failed
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HELPER_DIR="$REPO/mac/RemoteAgentDesktop"
PLUGIN_SRC="$REPO/mac/authorization-plugin/RemoteAgentLockedUse.m"
GRANT_SRC="$HELPER_DIR/Sources/RemoteAgentDesktopCore/Grant.swift"
CONFIG_SRC="$HELPER_DIR/Sources/RemoteAgentDesktopCore/LockedUseConfig.swift"
CHECK_SHIELD=0
CHECK_LOCK=0
FAILED=0
SKIPPED=0

for arg in "$@"; do
  case "$arg" in
    --check-shield) CHECK_SHIELD=1 ;;
    --check-lock) CHECK_LOCK=1 ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=$((FAILED + 1)); }
skip() { printf '  \033[33mskip\033[0m %s\n' "$1"; SKIPPED=$((SKIPPED + 1)); }
step() { printf '\n== %s\n' "$1"; }

step "environment"
if [ "$(uname -s)" != "Darwin" ]; then
  echo "  this script only runs on macOS (found $(uname -s))" >&2
  exit 1
fi
pass "macOS $(sw_vers -productVersion) on $(uname -m)"

for tool in swift clang codesign; do
  if command -v "$tool" >/dev/null 2>&1; then
    pass "$tool present"
  else
    fail "$tool missing — install the Xcode command line tools"
  fi
done

if command -v go >/dev/null 2>&1; then
  pass "go $(go version | awk '{print $3}')"
else
  fail "go missing"
fi

step "agent builds and its client tests pass"
if (cd "$REPO" && go build ./... 2>&1); then pass "go build"; else fail "go build"; fi
if (cd "$REPO" && go vet ./... 2>&1); then pass "go vet"; else fail "go vet"; fi
# The agent only forwards now; the safeguards are tested in the helper below.
if (cd "$REPO" && go test ./internal/computeruse/... ./internal/config/... ./internal/api/... 2>&1 | tail -5); then
  pass "client, config and API tests"
else
  fail "client, config and API tests"
fi

step "desktop helper builds and its tests pass"
if (cd "$HELPER_DIR" && swift build -c release 2>&1 | tail -3); then
  pass "remote-agent-desktop builds"
else
  fail "remote-agent-desktop does not build"
fi
# This is where the safeguards live: the action vocabulary and its bounds, the
# grant contract, and the Locked Use state machine.
if (cd "$HELPER_DIR" && swift test 2>&1 | tail -3); then
  pass "helper tests"
else
  fail "helper tests"
fi

HELPER="$HELPER_DIR/.build/release/remote-agent-desktop"

step "helper answers the read-only probes"
# These three are what the controller polls constantly. If any cannot answer,
# every safeguard built on it fails closed and Locked Use will not arm.
if [ -x "$HELPER" ]; then
  PROBES="$("$HELPER" --self-check 2>/dev/null)"
  for probe in locked idle_seconds engaged; do
    if printf '%s' "$PROBES" | grep -q "\"$probe\""; then
      pass "$probe answered"
    else
      fail "$probe did not answer (got: ${PROBES:-<empty>})"
    fi
  done
else
  fail "helper binary not found at $HELPER"
fi

step "accessibility permission"
# Synthetic events silently do nothing without this, which looks like a broken
# feature rather than a missing permission. Once the helper runs under launchd
# the grant belongs to the helper; this checks the terminal you are testing in.
AX="$(osascript -e 'tell application "System Events" to return UI elements enabled' 2>/dev/null)"
if [ "$AX" = "true" ]; then
  pass "accessibility is granted to this terminal"
else
  skip "accessibility not granted — grant it in System Settings > Privacy & Security > Accessibility, or synthetic input will do nothing"
fi

step "there is no unlock operation in the helper"
# The helper must not be able to unlock a Mac. Unlocking belongs to macOS and
# the authorization plug-in; a helper that could do it directly would make every
# controller safeguard bypassable.
if grep -rqiE '"(unlock|sac_?unlock)"|SACUnlock' "$HELPER_DIR/Sources"; then
  fail "the helper appears to expose an unlock operation"
else
  pass "no unlock operation in the helper"
fi

step "authorization plug-in builds"
# Ad-hoc signing is enough to prove it compiles and links. Installing requires a
# Developer ID identity and is a separate, explicit step.
if (cd "$REPO/mac/authorization-plugin" && ./build.sh --adhoc >/tmp/ra-plugin-build.log 2>&1); then
  pass "RemoteAgentLockedUse.bundle builds and ad-hoc signs"
else
  fail "plug-in build failed — see /tmp/ra-plugin-build.log"
  tail -20 /tmp/ra-plugin-build.log 2>/dev/null | sed 's/^/       /'
fi

step "plug-in and helper agree on the grant contract"
# These constants are duplicated across a language boundary. A silent drift
# means the agent mints grants the plug-in will always reject, and the only
# symptom is a Mac that never unlocks.
check_const() {
  local label="$1" objc_pattern="$2" swift_pattern="$3" swift_file="${4:-$GRANT_SRC}"
  local objc swift
  objc="$(grep -oE "$objc_pattern" "$PLUGIN_SRC" | head -1)"
  swift="$(grep -oE "$swift_pattern" "$swift_file" | head -1)"
  if [ -n "$objc" ] && [ -n "$swift" ]; then
    pass "$label (plug-in: $objc, helper: $swift)"
  else
    fail "$label could not be compared (plug-in: '${objc:-?}', helper: '${swift:-?}')"
  fi
}
check_const "grant version"   'kGrantVersion[^;]*= *[0-9]+'   'let version *= *[0-9]+'
check_const "max grant TTL"   'kMaxGrantTTL[^;]*= *[0-9.]+'   'let maxTTL: TimeInterval *= *[0-9]+'
check_const "max clock skew"  'kMaxClockSkew[^;]*= *[0-9.]+'  'let maxClockSkew: TimeInterval *= *[0-9]+'
check_const "grant purpose"   'screensaver-unlock'            'screensaver-unlock'
check_const "public key size" 'kPublicKeyBytes[^;]*= *[0-9]+' 'let publicKeyBytes *= *[0-9]+'

PLUGIN_DIR_CONST="$(grep -oE '/Library/Application Support/remote-agent/locked-use' "$PLUGIN_SRC" | head -1)"
HELPER_DIR_CONST="$(grep -oE '/Library/Application Support/remote-agent/locked-use' "$CONFIG_SRC" | head -1)"
if [ -n "$PLUGIN_DIR_CONST" ] && [ "$PLUGIN_DIR_CONST" = "$HELPER_DIR_CONST" ]; then
  pass "grant directory matches on both sides"
else
  fail "grant directory differs — the agent would publish grants where the plug-in never looks"
fi

step "plug-in uses only public Security API"
# A mechanism bundle is loaded inside authd, in the screensaver-unlock right. If
# it binds a symbol Apple later drops, it stops loading *there* — a Mac that
# will not unlock. So every Security constant it names must be declared in a
# public SDK header, not merely exported by Security.tbd. Ed25519's SecKey
# constants are exactly that trap: exported, undeclared, and they compile only
# if you declare them yourself.
SDK_PATH="$(xcrun --sdk macosx --show-sdk-path 2>/dev/null)"
SEC_HEADERS="$SDK_PATH/System/Library/Frameworks/Security.framework/Versions/A/Headers"
[ -d "$SEC_HEADERS" ] || SEC_HEADERS="$SDK_PATH/System/Library/Frameworks/Security.framework/Headers"
if [ -d "$SEC_HEADERS" ]; then
  UNDECLARED=""
  for sym in $(grep -oE 'kSec[A-Za-z0-9]+' "$PLUGIN_SRC" | sort -u); do
    grep -rqE "\b$sym\b" "$SEC_HEADERS" 2>/dev/null || UNDECLARED="$UNDECLARED $sym"
  done
  if [ -z "$UNDECLARED" ]; then
    pass "every kSec* constant the plug-in names is declared in a public header"
  else
    fail "plug-in references SPI (exported but undeclared):$UNDECLARED"
  fi
else
  skip "cannot locate Security.framework headers in the SDK"
fi

step "a helper-minted grant verifies in the plug-in's own verifier"
# The contract has two implementations in two languages and only the ObjC one
# enforces anything. Nothing in either test suite can tell "they agree" apart
# from "each is self-consistent and rejects the other" — which fails silently:
# the agent mints, the plug-in refuses, and the Mac simply never unlocks. This
# is the one place the two are run against each other, so it happens here.
VECTOR="$(mktemp -t ra-interop-vector)"
CHECKER="$(mktemp -d -t ra-interop)/interop_check"
if (cd "$HELPER_DIR" && RA_INTEROP_VECTOR_OUT="$VECTOR" \
      swift test --filter testInteropVector >/dev/null 2>&1) && [ -s "$VECTOR" ]; then
  if clang -fobjc-arc -Wno-unused-function -framework Foundation -framework Security \
       -o "$CHECKER" "$REPO/mac/authorization-plugin/interop_check.m" 2>/tmp/ra-interop-build.log; then
    PUB="$(sed -n 1p "$VECTOR")"
    PAYLOAD="$(sed -n 2p "$VECTOR")"
    SIG="$(sed -n 3p "$VECTOR")"
    WRONG_PUB="$(sed -n 4p "$VECTOR")"
    if "$CHECKER" "$PUB" "$PAYLOAD" "$SIG"; then
      pass "plug-in verifier accepts a grant minted by the helper"
    else
      fail "plug-in verifier REJECTED a valid helper-minted grant — the two sides disagree"
    fi
    # Without a case it must refuse, a verifier that always allowed would pass.
    if "$CHECKER" "$WRONG_PUB" "$PAYLOAD" "$SIG"; then
      fail "plug-in verifier accepted a grant under the wrong public key"
    else
      pass "plug-in verifier refuses a grant signed by another key"
    fi
  else
    fail "interop harness did not build — see /tmp/ra-interop-build.log"
    tail -20 /tmp/ra-interop-build.log 2>/dev/null | sed 's/^/       /'
  fi
else
  fail "could not mint an interop vector from the helper"
fi
rm -f "$VECTOR"
rm -rf "$(dirname "$CHECKER")"

if [ "$CHECK_SHIELD" = "1" ]; then
  step "display shield (disruptive)"
  # Raising and dropping the shield is not a socket operation: a shield any
  # connected process could release is one that can be taken down while a
  # window is open. It is reachable only by running this binary directly.
  SHIELD="$("$HELPER" --check-shield 2>/dev/null | tail -1)"
  if printf '%s' "$SHIELD" | grep -q '"ok":true'; then
    pass "shield engaged, reported live, and released ($SHIELD)"
  else
    fail "shield did not engage and report live (got: ${SHIELD:-<empty>})"
  fi
else
  skip "display shield check — pass --check-shield to run it (briefly covers your screen)"
fi

if [ "$CHECK_LOCK" = "1" ]; then
  step "screen lock (disruptive)"
  echo "  locking in 3s; log back in and re-run to see the result"
  sleep 3
  LOCKED="$("$HELPER" --check-lock 2>/dev/null | tail -1)"
  if printf '%s' "$LOCKED" | grep -q '"ok":true'; then
    pass "lock succeeded"
  else
    fail "lock failed (got: ${LOCKED:-<empty>}) — SACLockScreenImmediate may be unavailable on this macOS version"
  fi
else
  skip "screen lock check — pass --check-lock to run it (locks your screen)"
fi

step "summary"
if [ "$FAILED" -eq 0 ]; then
  printf '  \033[32mall attempted checks passed\033[0m (%d skipped)\n' "$SKIPPED"
  cat <<'NEXT'

  Next, in order:
    1. Re-run with --check-shield --check-lock to exercise the disruptive paths.
    2. Enable computer_use.enabled in config.json, start the helper, and try
       /computer_use/action (screen.capture, pointer.move) with locked_use off.
    3. Only then read docs/computer-use-locked-user.md "未验证事项" and install
       the plug-in on a spare Mac or VM first. It changes how the Mac unlocks.
NEXT
  exit 0
fi
printf '  \033[31m%d check(s) failed\033[0m (%d skipped)\n' "$FAILED" "$SKIPPED"
echo "  Do not install the authorization plug-in until these pass."
exit 1
