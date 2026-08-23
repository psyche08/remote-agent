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

# Keep compiler caches outside user Library paths. Besides making the probe
# reproducible under launch/sandboxed shells, this prevents a read-only SwiftPM
# or Go cache from being reported as a product build failure.
: "${GOCACHE:=/tmp/agenthalo-go-cache}"
: "${CLANG_MODULE_CACHE_PATH:=/tmp/agenthalo-clang-cache}"
: "${SWIFT_MODULECACHE_PATH:=/tmp/agenthalo-swift-cache}"
export GOCACHE CLANG_MODULE_CACHE_PATH SWIFT_MODULECACHE_PATH
mkdir -p "$GOCACHE" "$CLANG_MODULE_CACHE_PATH" "$SWIFT_MODULECACHE_PATH"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HELPER_DIR="$REPO/mac/RemoteAgentDesktop"
PLUGIN_SRC="$REPO/mac/authorization-plugin/AgentHaloLockedUse.m"
GRANT_SRC="$HELPER_DIR/Sources/AgentHaloDesktopCore/Grant.swift"
CONFIG_SRC="$HELPER_DIR/Sources/AgentHaloDesktopCore/LockedUseConfig.swift"
SHIELD_SRC="$HELPER_DIR/Sources/AgentHaloDesktopCore/DisplayShield.swift"
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
if (cd "$HELPER_DIR" && swift build --disable-sandbox -c release 2>&1 | tail -3); then
  pass "agenthalo-desktop builds"
else
  fail "agenthalo-desktop does not build"
fi
# This is where the safeguards live: the action vocabulary and its bounds, the
# grant contract, and the Locked Use state machine.
if (cd "$HELPER_DIR" && swift test --disable-sandbox 2>&1 | tail -3); then
  pass "helper tests"
else
  fail "helper tests"
fi

HELPER="$HELPER_DIR/.build/release/agenthalo-desktop"

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

step "lock-screen authorization contains no credential path"
# Locked Use asks loginwindow to evaluate the screensaver right; the signed
# Authorization Plug-in is the branch that may authorize it.  The helper must
# never store/inject a login password or call a private direct-unlock primitive.
if grep -rqiE 'import LocalAuthentication|setCredential|kAuthorizationEnvironmentPassword|SACUnlock' \
     "$HELPER_DIR/Sources"; then
  fail "the helper contains a credential or direct-unlock path"
else
  pass "no password storage/injection or direct-unlock primitive"
fi

if grep -q 'captureSharingType: NSWindow.SharingType = .readOnly' "$SHIELD_SRC" && \
   ! grep -q 'window.sharingType = .none' "$SHIELD_SRC"; then
  pass "ordinary capture clients retain the black display shield"
else
  fail "display shield is omitted from ordinary screen captures"
fi
INTERACTOR="$HELPER_DIR/Sources/AgentHaloDesktopCore/LockScreenAuthorizationInteractor.swift"
if grep -q 'UserPasswordTextField' "$INTERACTOR" 2>/dev/null && \
   grep -q 'com.apple.loginwindow' "$INTERACTOR" 2>/dev/null && \
   grep -q '/System/Library/CoreServices/loginwindow.app' "$INTERACTOR" 2>/dev/null && \
   grep -q '/System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow' "$INTERACTOR" 2>/dev/null && \
   grep -q 'NSRunningApplication.runningApplications' "$INTERACTOR" 2>/dev/null && \
   grep -q 'requireSameExactLoginwindow' "$INTERACTOR" 2>/dev/null && \
   grep -q 'AXUIElementCreateApplication(processIdentifier)' "$INTERACTOR" 2>/dev/null && \
   grep -q 'AXUIElementGetPid' "$INTERACTOR" 2>/dev/null && \
   grep -q 'kAXFocusedUIElementAttribute' "$INTERACTOR" 2>/dev/null && \
   grep -q 'kAXFocusedWindowAttribute' "$INTERACTOR" 2>/dev/null && \
   grep -q 'kAXWindowsAttribute' "$INTERACTOR" 2>/dev/null && \
   grep -q 'discoveryTimeout: TimeInterval = 8' "$INTERACTOR" 2>/dev/null && \
   grep -q 'try prepareGrant()' "$INTERACTOR" 2>/dev/null && \
   grep -q 'submissionDeadline' "$INTERACTOR" 2>/dev/null && \
   grep -q 'preflightEmptySubmission' "$INTERACTOR" 2>/dev/null && \
   grep -q 'preflightConfirmAction' "$INTERACTOR" 2>/dev/null && \
   grep -q 'revalidatePreparedFieldBeforeGrant' "$INTERACTOR" 2>/dev/null && \
   grep -q 'revalidateRetainedFieldIdentityAfterGrant' "$INTERACTOR" 2>/dev/null && \
   grep -q 'performCredentialFreeFieldReadiness' "$INTERACTOR" 2>/dev/null && \
   grep -q 'performGrantGatedSubmission' "$INTERACTOR" 2>/dev/null && \
   grep -q 'writeEmptySubmission' "$INTERACTOR" 2>/dev/null && \
   grep -q 'performSingleConfirmedSubmission' "$INTERACTOR" 2>/dev/null && \
   grep -q 'AXUIElementIsAttributeSettable' "$INTERACTOR" 2>/dev/null && \
   grep -q 'AXUIElementCopyActionNames' "$INTERACTOR" 2>/dev/null && \
   grep -q 'AXUIElementPerformAction' "$INTERACTOR" 2>/dev/null && \
   grep -q 'kAXConfirmAction' "$INTERACTOR" 2>/dev/null && \
   grep -q 'kAXValueAttribute as CFString, "" as CFString' "$INTERACTOR" 2>/dev/null; then
  pass "exact-loginwindow retained-field pregrant checks and single Set+AXConfirm wiring present"
else
  fail "the bounded exact-PID two-phase credential-free loginwindow AX authorization interactor is missing"
fi
CONFIRM_PERFORM_CALLS="$(grep -c 'AXUIElementPerformAction(' "$INTERACTOR" 2>/dev/null || true)"
if [ "$CONFIRM_PERFORM_CALLS" -eq 1 ] && \
   grep -q 'field, kAXConfirmAction as CFString' "$INTERACTOR" 2>/dev/null && \
   ! grep -qE 'kAXPressAction|kAXButtonRole|kAXRoleAttribute|CGEventCreateKeyboardEvent|CGEventKeyboardSetUnicodeString|CGEventPost|IOHIDPostEvent' \
     "$INTERACTOR" 2>/dev/null; then
  pass "lock-screen authorization performs only one retained-field AXConfirm with no input fallback"
else
  fail "lock-screen authorization must use exactly one field AXConfirm and no Press/button/keyboard/HID fallback"
fi
if grep -q 'kAXFocusedApplicationAttribute' "$INTERACTOR" 2>/dev/null || \
   grep -qE '\.activate[[:space:]]*\(|activateIgnoringOtherApps|SetFrontProcess' "$INTERACTOR" 2>/dev/null; then
  fail "lock-screen authorization trusts focused-app routing or activates/fronts a process"
else
  pass "no focused-app routing or process activation/fronting API is statically present in the lock-screen interactor"
fi

step "authorization plug-in builds"
# Ad-hoc signing is enough to prove it compiles and links. Installing requires a
# Developer ID identity and is a separate, explicit step.
if (cd "$REPO/mac/authorization-plugin" && ./build.sh --adhoc >/tmp/agenthalo-plugin-build.log 2>&1); then
  pass "AgentHaloLockedUse.bundle builds and ad-hoc signs"
else
  fail "plug-in build failed — see /tmp/agenthalo-plugin-build.log"
  tail -20 /tmp/agenthalo-plugin-build.log 2>/dev/null | sed 's/^/       /'
fi

step "plug-in and helper agree on the grant contract"
# These constants are duplicated across a language boundary. A silent drift
# means the agent mints grants the plug-in will always reject, and the only
# symptom is a Mac that never unlocks.
check_const() {
  local label="$1" objc_pattern="$2" swift_pattern="$3" swift_file="${4:-$GRANT_SRC}"
  local objc swift objc_value swift_value equal=0
  objc="$(grep -oE "$objc_pattern" "$PLUGIN_SRC" | head -1)"
  swift="$(grep -oE "$swift_pattern" "$swift_file" | head -1)"
  if [ -z "$objc" ] || [ -z "$swift" ]; then
    fail "$label could not be compared (plug-in: '${objc:-?}', helper: '${swift:-?}')"
    return
  fi
  objc_value="$(printf '%s' "$objc" | sed -E 's/.*= *//; s/[;[:space:]]+$//')"
  swift_value="$(printf '%s' "$swift" | sed -E 's/.*= *//; s/[;[:space:]]+$//')"
  if [[ "$objc_value" =~ ^[0-9]+([.][0-9]+)?$ ]] && \
     [[ "$swift_value" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    awk -v a="$objc_value" -v b="$swift_value" 'BEGIN { exit(a == b ? 0 : 1) }' && equal=1
  elif [ "$objc_value" = "$swift_value" ]; then
    equal=1
  fi
  if [ "$equal" = "1" ]; then
    pass "$label matches ($objc_value)"
  else
    fail "$label differs (plug-in: $objc_value, helper: $swift_value)"
  fi
}
check_const "grant version"   'kGrantVersion[^;]*= *[0-9]+'   'let version *= *[0-9]+'
check_const "max grant TTL"   'kMaxGrantTTL[^;]*= *[0-9.]+'   'let maxTTL: TimeInterval *= *[0-9]+'
check_const "max clock skew"  'kMaxClockSkew[^;]*= *[0-9.]+'  'let maxClockSkew: TimeInterval *= *[0-9]+'
check_const "grant purpose"   'screensaver-unlock'            'screensaver-unlock'
check_const "public key size" 'kPublicKeyBytes[^;]*= *[0-9]+' 'let publicKeyBytes *= *[0-9]+'

PLUGIN_NONCE_HEX="$(grep -oE 'kNonceHexLen[^;]*= *[0-9]+' "$PLUGIN_SRC" | sed -E 's/.*= *//' | head -1)"
HELPER_NONCE_BYTES="$(grep -oE 'let nonceBytes *= *[0-9]+' "$GRANT_SRC" | sed -E 's/.*= *//' | head -1)"
if [[ "$PLUGIN_NONCE_HEX" =~ ^[0-9]+$ ]] && \
   [[ "$HELPER_NONCE_BYTES" =~ ^[0-9]+$ ]] && \
   [ "$PLUGIN_NONCE_HEX" -eq $((HELPER_NONCE_BYTES * 2)) ]; then
  pass "nonce/receipt size matches ($HELPER_NONCE_BYTES bytes = $PLUGIN_NONCE_HEX hex characters)"
else
  fail "nonce size differs (plug-in hex: ${PLUGIN_NONCE_HEX:-?}, helper bytes: ${HELPER_NONCE_BYTES:-?})"
fi

PLUGIN_DIR_CONST="$(grep -oE '/Library/Application Support/AgentHalo/locked-use' "$PLUGIN_SRC" | head -1)"
HELPER_DIR_CONST="$(grep -oE '/Library/Application Support/AgentHalo/locked-use' "$CONFIG_SRC" | head -1)"
if [ -n "$PLUGIN_DIR_CONST" ] && [ "$PLUGIN_DIR_CONST" = "$HELPER_DIR_CONST" ]; then
  pass "grant directory matches on both sides"
else
  fail "grant directory differs — the agent would publish grants where the plug-in never looks"
fi

PLUGIN_RECEIPT="$(grep -oE 'AGENTHALO_LOCKED_USE_DIR \"/receipt\"' "$PLUGIN_SRC" | head -1)"
HELPER_RECEIPT="$(grep -oE 'receiptFileName *= *\"receipt\"' "$GRANT_SRC" | head -1)"
if [ -n "$PLUGIN_RECEIPT" ] && [ -n "$HELPER_RECEIPT" ]; then
  pass "exact-nonce receipt path matches on both sides"
else
  fail "authorization receipt contract is missing or differs"
fi

PLUGIN_PENDING_RECEIPT="$(grep -oE 'AGENTHALO_LOCKED_USE_DIR "/receipt.pending"' "$PLUGIN_SRC" | head -1)"
HELPER_PENDING_RECEIPT="$(grep -oE 'pendingReceiptFileName *= *"receipt.pending"' "$GRANT_SRC" | head -1)"
if [ -n "$PLUGIN_PENDING_RECEIPT" ] && [ -n "$HELPER_PENDING_RECEIPT" ]; then
  pass "pre-Allow exact-nonce pending receipt matches on both sides"
else
  fail "pending authorization receipt contract is missing or differs"
fi

PLUGIN_COMPLETION_RECEIPT="$(grep -oE 'AGENTHALO_LOCKED_USE_DIR "/receipt.complete"' "$PLUGIN_SRC" | head -1)"
HELPER_COMPLETION_RECEIPT="$(grep -oE 'completionReceiptFileName *= *"receipt.complete"' "$GRANT_SRC" | head -1)"
if [ -n "$PLUGIN_COMPLETION_RECEIPT" ] && [ -n "$HELPER_COMPLETION_RECEIPT" ]; then
  pass "mechanism-terminal exact-nonce receipt matches on both sides"
else
  fail "terminal authorization receipt contract is missing or differs"
fi

DEACTIVATE_BODY="$(sed -n '/static OSStatus MechanismDeactivate/,/static OSStatus MechanismDestroy/p' "$PLUGIN_SRC")"
DESTROY_BODY="$(sed -n '/static OSStatus MechanismDestroy/,/static OSStatus PluginDestroy/p' "$PLUGIN_SRC")"
if ! grep -q 'AgentHaloPublishNonceProof' <<<"$DEACTIVATE_BODY" && \
   grep -q 'mechanism->completionEligible = NO' <<<"$DEACTIVATE_BODY" && \
   grep -q 'memset(mechanism->completionNonce' <<<"$DEACTIVATE_BODY" && \
   grep -q 'AGENTHALO_COMPLETION_RECEIPT_PATH' <<<"$DESTROY_BODY" && \
   grep -q 'mechanism->completionEligible = NO' "$PLUGIN_SRC" && \
   grep -q 'memset(mechanism->completionNonce' "$PLUGIN_SRC"; then
  pass "only successful mechanism Destroy can publish terminal proof and re-invoke clears eligibility"
else
  fail "authorization terminal proof lifecycle is not fail-closed"
fi

if grep -q 'flock(fd, LOCK_SH)' "$PLUGIN_SRC" && \
   grep -q 'flock(fd, LOCK_EX | LOCK_NB)' "$GRANT_SRC"; then
  pass "grant verification and bounded withdrawal share an advisory fd lock"
else
  fail "grant verifier/withdrawal serialization lock is missing"
fi

if grep -q 'case consoleUID = "console_uid"' "$GRANT_SRC" && \
   grep -q 'case consoleUsername = "console_username"' "$GRANT_SRC" && \
   grep -q 'claims\[@"console_uid"\]' "$PLUGIN_SRC" && \
   grep -q 'claims\[@"console_username"\]' "$PLUGIN_SRC" && \
   grep -q 'GetContextValue' "$PLUGIN_SRC" && \
   grep -q 'kAuthorizationEnvironmentUsername' "$PLUGIN_SRC"; then
  pass "grant console uid/username claims match the public authorization context contract"
else
  fail "cross-user grant binding is missing or differs across helper and plug-in"
fi

if grep -Fq 'expectedDevice.length == 0 || ![device isEqualToString:expectedDevice]' "$PLUGIN_SRC" && \
   grep -Fq 'guard !deviceID.isEmpty, payload.deviceID == deviceID' "$GRANT_SRC"; then
  pass "missing device binding fails closed in plug-in and helper verifier"
else
  fail "device binding may be skipped when its provisioned value is unavailable"
fi

step "screensaver authorization right has a safe branch shape"
# Reading authorizationdb is non-mutating.  The installer only understands the
# exact rule/k-of-n form used by modern macOS, plus the one unambiguous stock
# shape where the sole password fallback has an omitted default. Discovering a
# different shape before installation is much safer than guessing how authd
# will evaluate it.
RIGHT_PLIST="$(mktemp -t agenthalo-screensaver-right)"
if security authorizationdb read system.login.screensaver >"$RIGHT_PLIST" 2>/dev/null; then
  RIGHT_SHAPE="$(/usr/bin/python3 - "$RIGHT_PLIST" <<'PYEOF'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    right = plistlib.load(f)
rules = right.get("rule")
if right.get("class") != "rule" or not isinstance(rules, list):
    sys.exit(1)
if "use-login-window-ui" not in rules:
    sys.exit(1)
if "k-of-n" not in right:
    if rules != ["use-login-window-ui"]:
        sys.exit(1)
    print("normalizable-single-password-fallback")
elif type(right.get("k-of-n")) is int and right.get("k-of-n") == 1:
    print("canonical-1-of-n")
else:
    sys.exit(1)
PYEOF
  )" || RIGHT_SHAPE="unsupported"
  if [ "$RIGHT_SHAPE" = "canonical-1-of-n" ]; then
    pass "system.login.screensaver is a 1-of-n rule with the normal password fallback"
  elif [ "$RIGHT_SHAPE" = "normalizable-single-password-fallback" ]; then
    pass "system.login.screensaver is the single password fallback; installer can safely make k-of-n=1 explicit"
  else
    fail "system.login.screensaver has an unsupported shape; do not install the plug-in"
  fi
else
  fail "could not read system.login.screensaver"
fi
rm -f "$RIGHT_PLIST"

PLUGIN_RULE="dev.linsheng.agenthalo.locked-use"
RULE_PLIST="$(mktemp -t agenthalo-locked-use-rule)"
if security authorizationdb read "$PLUGIN_RULE" >"$RULE_PLIST" 2>/dev/null; then
  if /usr/bin/python3 - "$RULE_PLIST" <<'PYEOF'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    rule = plistlib.load(f)
timeout = rule.get("timeout")
timeout_ok = timeout is None or (type(timeout) is int and timeout == 0)
ok = (rule.get("class") == "evaluate-mechanisms"
      and rule.get("mechanisms") == ["AgentHaloLockedUse:invoke,privileged"]
      and rule.get("shared") is False
      and timeout_ok
      and type(rule.get("tries")) is int
      and rule.get("tries") == 1)
raise SystemExit(0 if ok else 1)
PYEOF
  then
    pass "$PLUGIN_RULE is non-shared, single-use, and names only the expected mechanism"
  else
    fail "$PLUGIN_RULE may cache/reuse authorization or has an unexpected mechanism"
  fi
else
  skip "$PLUGIN_RULE is not installed yet"
fi
rm -f "$RULE_PLIST"

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
  if grep -q 'kAuthorizationEnvironmentUsername' \
       "$SEC_HEADERS/AuthorizationTags.h" 2>/dev/null && \
     grep -q 'GetContextValue' \
       "$SEC_HEADERS/AuthorizationPlugin.h" 2>/dev/null; then
    pass "username context key and GetContextValue callback are public Security API"
  else
    fail "authorization username context API is not declared by this SDK"
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
VECTOR="$(mktemp -t agenthalo-interop-vector)"
CHECKER="$(mktemp -d -t agenthalo-interop)/interop_check"
if (cd "$HELPER_DIR" && AGENTHALO_INTEROP_VECTOR_OUT="$VECTOR" \
      swift test --filter testInteropVector >/dev/null 2>&1) && [ -s "$VECTOR" ]; then
  if clang -fobjc-arc -Wno-unused-function -framework Foundation -framework Security \
       -o "$CHECKER" "$REPO/mac/authorization-plugin/interop_check.m" 2>/tmp/agenthalo-interop-build.log; then
    PUB="$(sed -n 1p "$VECTOR")"
    PAYLOAD="$(sed -n 2p "$VECTOR")"
    SIG="$(sed -n 3p "$VECTOR")"
    WRONG_PUB="$(sed -n 4p "$VECTOR")"
    CONSOLE_USER="$(sed -n 5p "$VECTOR")"
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
    if [ -n "$CONSOLE_USER" ] && \
       "$CHECKER" --claims-user "$PAYLOAD" "$CONSOLE_USER"; then
      pass "plug-in verifier accepts the signed active console identity"
    else
      fail "plug-in verifier rejected the helper's signed console identity"
    fi
    if "$CHECKER" --claims-user "$PAYLOAD" "__remote_agent_wrong_user__"; then
      fail "plug-in verifier accepted a grant for the wrong authorization username"
    else
      pass "plug-in verifier refuses a grant from another user session"
    fi
  else
    fail "interop harness did not build — see /tmp/agenthalo-interop-build.log"
    tail -20 /tmp/agenthalo-interop-build.log 2>/dev/null | sed 's/^/       /'
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
    2. Start a fresh Claude logical session and verify the primary
       desktop_computer_use route with get_app_state -> one mutation ->
       get_app_state on the ordinary desktop. The Claude CLI is only a
       pre-mutation capability fallback.
    3. Follow mac/RemoteAgentDesktop/SETUP-locked-unlock.md from the signing
       checks onward. Install the plug-in on a spare Mac or VM first: it changes
       the system.login.screensaver authorization right.
NEXT
  exit 0
fi
printf '  \033[31m%d check(s) failed\033[0m (%d skipped)\n' "$FAILED" "$SKIPPED"
echo "  Do not install the authorization plug-in until these pass."
exit 1
