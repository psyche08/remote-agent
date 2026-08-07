#!/usr/bin/env bash
# Preflight for computer use + Locked Use on the target Mac.
#
# Everything here is what CI and a Linux container cannot check: the Swift
# worker and the Objective-C authorization plug-in only compile and run on
# macOS. Run this first on the machine you intend to use, before installing
# anything.
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
SWIFT_WORKER="$REPO/scripts/computer_use.swift"
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

step "go build, vet and tests"
if (cd "$REPO" && go build ./... 2>&1); then pass "go build"; else fail "go build"; fi
if (cd "$REPO" && go vet ./... 2>&1); then pass "go vet"; else fail "go vet"; fi
# Only the packages this feature owns; the rest of the suite is unrelated here.
if (cd "$REPO" && go test ./internal/computeruse/... ./internal/config/... 2>&1 | tail -5); then
  pass "computeruse + config tests"
else
  fail "computeruse + config tests"
fi

step "swift worker compiles"
# Type-check without running, so a syntax or API error surfaces as itself
# rather than as a runtime failure inside one operation.
if swiftc -typecheck "$SWIFT_WORKER" 2>&1; then
  pass "computer_use.swift type-checks"
else
  fail "computer_use.swift does not type-check"
fi

# run_worker <json> -> prints stdout, returns worker's success
run_worker() {
  swift "$SWIFT_WORKER" "$1" 2>/dev/null
}

step "swift worker read-only operations"
# These three are what the controller polls constantly. If any of them cannot
# answer, every safeguard built on it fails closed and Locked Use will not arm.
LOCK_STATE="$(run_worker '{"op":"lock_state"}')"
if printf '%s' "$LOCK_STATE" | grep -q '"ok":true'; then
  pass "lock_state -> $LOCK_STATE"
else
  fail "lock_state did not answer (got: ${LOCK_STATE:-<empty>})"
fi

IDLE="$(run_worker '{"op":"idle_seconds"}')"
if printf '%s' "$IDLE" | grep -q '"ok":true'; then
  pass "idle_seconds -> $IDLE"
else
  fail "idle_seconds did not answer (got: ${IDLE:-<empty>})"
fi

SHIELD_STATE="$(run_worker '{"op":"shield_state"}')"
if printf '%s' "$SHIELD_STATE" | grep -q '"ok":true'; then
  pass "shield_state -> $SHIELD_STATE"
else
  fail "shield_state did not answer (got: ${SHIELD_STATE:-<empty>})"
fi

step "swift worker rejects unknown input"
# The action set is closed. A worker that accepted an unknown op would mean the
# Go-side validation is the only thing standing between a caller and the desktop.
BAD="$(run_worker '{"op":"definitely-not-an-op"}')"
if printf '%s' "$BAD" | grep -q '"ok":false'; then
  pass "unknown op refused"
else
  fail "unknown op was not refused (got: ${BAD:-<empty>})"
fi
BAD_ACTION="$(run_worker '{"op":"action","action":"shell.exec"}')"
if printf '%s' "$BAD_ACTION" | grep -q '"ok":false'; then
  pass "unknown action refused"
else
  fail "unknown action was not refused (got: ${BAD_ACTION:-<empty>})"
fi

step "there is no unlock operation"
# The worker must not be able to unlock a Mac. Unlocking belongs to macOS and
# the authorization plug-in; a worker that could do it directly would make every
# controller safeguard bypassable.
if grep -qiE '"(unlock|sac_?unlock)"|SACUnlock' "$SWIFT_WORKER"; then
  fail "the worker appears to expose an unlock operation"
else
  pass "no unlock operation in the worker"
fi

step "accessibility permission"
# Synthetic events silently do nothing without this, which would look like a
# broken feature rather than a missing permission.
AX="$(osascript -e 'tell application "System Events" to return UI elements enabled' 2>/dev/null)"
if [ "$AX" = "true" ]; then
  pass "accessibility is granted to this terminal"
else
  skip "accessibility not granted — grant it in System Settings > Privacy & Security > Accessibility, or synthetic input will do nothing"
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

step "plug-in and Go agree on the grant contract"
# These constants are duplicated across a language boundary. A silent drift
# means the agent mints grants the plug-in will always reject.
check_const() {
  local label="$1" objc_pattern="$2" go_pattern="$3"
  local objc go
  objc="$(grep -oE "$objc_pattern" "$REPO/mac/authorization-plugin/RemoteAgentLockedUse.m" | head -1)"
  go="$(grep -oE "$go_pattern" "$REPO/internal/computeruse/grant.go" | head -1)"
  if [ -n "$objc" ] && [ -n "$go" ]; then
    pass "$label (plug-in: $objc, go: $go)"
  else
    fail "$label could not be compared (plug-in: '${objc:-?}', go: '${go:-?}')"
  fi
}
check_const "grant version"  'kGrantVersion[^;]*= *[0-9]+'      'GrantVersion *= *[0-9]+'
check_const "max grant TTL"  'kMaxGrantTTL[^;]*= *[0-9.]+'      'MaxGrantTTL *= *[0-9]+ *\* *time\.Second'
check_const "max clock skew" 'kMaxClockSkew[^;]*= *[0-9.]+'     'MaxClockSkew *= *[0-9]+ *\* *time\.Second'
check_const "grant purpose"  'screensaver-unlock'               'screensaver-unlock'

PLUGIN_DIR_CONST="$(grep -oE '/Library/Application Support/remote-agent/locked-use' "$REPO/mac/authorization-plugin/RemoteAgentLockedUse.m" | head -1)"
GO_DIR_CONST="$(grep -oE '/Library/Application Support/remote-agent/locked-use' "$REPO/internal/computeruse/locked.go" | head -1)"
if [ -n "$PLUGIN_DIR_CONST" ] && [ "$PLUGIN_DIR_CONST" = "$GO_DIR_CONST" ]; then
  pass "grant directory matches on both sides"
else
  fail "grant directory differs — the agent would publish grants where the plug-in never looks"
fi

if [ "$CHECK_SHIELD" = "1" ]; then
  step "display shield (disruptive)"
  ENGAGE="$(run_worker '{"op":"shield_engage"}')"
  if printf '%s' "$ENGAGE" | grep -q '"ok":true'; then
    sleep 2
    STATE="$(run_worker '{"op":"shield_state"}')"
    run_worker '{"op":"shield_release"}' >/dev/null
    if printf '%s' "$STATE" | grep -q '"engaged":true'; then
      pass "shield engaged, reported live, and released ($ENGAGE)"
    else
      fail "shield engaged but did not report as live (got: $STATE)"
    fi
  else
    fail "shield could not engage (got: ${ENGAGE:-<empty>})"
  fi
else
  skip "display shield check — pass --check-shield to run it (briefly covers your screen)"
fi

if [ "$CHECK_LOCK" = "1" ]; then
  step "screen lock (disruptive)"
  echo "  locking in 3s; log back in and re-run to see the result"
  sleep 3
  LOCKED="$(run_worker '{"op":"lock"}')"
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
    2. Enable computer_use.enabled in config.json and try /computer_use/action
       (screen.capture, pointer.move) with locked_use still off.
    3. Only then read docs/computer-use-locked-user.md "未验证事项" and install
       the plug-in on a spare Mac or VM first. It changes how the Mac unlocks.
NEXT
  exit 0
fi
printf '  \033[31m%d check(s) failed\033[0m (%d skipped)\n' "$FAILED" "$SKIPPED"
echo "  Do not install the authorization plug-in until these pass."
exit 1
