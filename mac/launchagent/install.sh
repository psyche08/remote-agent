#!/usr/bin/env bash
# Installs the AgentHalo desktop helper as a per-user LaunchAgent.
#
# It has to be a LaunchAgent, in the user's own GUI session, for two reasons
# that are not interchangeable with "the agent spawns it":
#
#   * CoreGraphics and AppKit need a GUI session. The display shield is real
#     windows; a daemon outside an Aqua session cannot hold them.
#   * TCC attributes Accessibility and Screen Recording to the *responsible*
#     process. A helper spawned by the agent inherits the agent's
#     responsibility, so grants land on the wrong identity and synthetic input
#     silently does nothing. Started by launchd, the helper is its own
#     responsible process with its own stable grants.
#
# Run as the user who is logged in at the Mac — not with sudo.
#
# Usage:
#   ./install.sh --config /path/to/config.json [--helper /path/to/binary]
#   ./install.sh --uninstall
set -euo pipefail

LABEL="dev.linsheng.agenthalo.desktop"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
SUPPORT_DIR="$HOME/Library/Application Support/AgentHalo"
HELPER="$SUPPORT_DIR/bin/agenthalo-desktop"
SOCKET="$SUPPORT_DIR/desktop.sock"
LOG="$HOME/Library/Logs/AgentHalo/agenthalo-desktop.log"
CONFIG=""
UNINSTALL=0

while [ $# -gt 0 ]; do
  case "$1" in
    --config) shift; CONFIG="${1:-}" ;;
    --helper) shift; HELPER="${1:-}" ;;
    --socket) shift; SOCKET="${1:-}" ;;
    --uninstall) UNINSTALL=1 ;;
    -h|--help) sed -n '2,22p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(uname -s)" = "Darwin" ] || { echo "this only applies to macOS" >&2; exit 1; }
if [ "$(id -u)" = "0" ]; then
  echo "run this as the logged-in user, not with sudo: a LaunchAgent belongs to a user session" >&2
  exit 2
fi

TARGET="gui/$(id -u)"

if [ "$UNINSTALL" = "1" ]; then
  # launchctl bootout is not a safety handshake: it may kill a helper before a
  # Locked Use quarantine has relocked and released its shield. The signed agent
  # must first complete prepare_restart. Once the job is unloaded, removing its
  # definition and runtime socket is safe and idempotent.
  if launchctl print "$TARGET/$LABEL" >/dev/null 2>&1; then
    echo "$LABEL is still loaded; refusing an unsafe uninstall" >&2
    echo "use the signed AgentHalo update/stop path to complete prepare_restart first" >&2
    exit 1
  fi
  rm -f "$PLIST"
  # The socket is a runtime artifact of a process that is now gone; leaving it
  # behind would make a dead helper look reachable until the first connect.
  rm -f "$SOCKET"
  echo "==> removed $LABEL"
  exit 0
fi

[ -n "$CONFIG" ] || { echo "--config is required (the agent's config.json)" >&2; exit 2; }
[ -f "$CONFIG" ] || { echo "config not found: $CONFIG" >&2; exit 2; }
[ -x "$HELPER" ] || { echo "helper not found or not executable: $HELPER" >&2; exit 2; }

# A helper whose signature does not verify cannot hold TCC grants under a
# stable identity, so it would appear installed and never be able to type.
if ! codesign --verify --strict "$HELPER" 2>/dev/null; then
  echo "$HELPER has no valid signature; refusing to register an unstable TCC identity" >&2
  exit 1
fi
HELPER_SIGN_INFO="$(codesign -d --verbose=4 "$HELPER" 2>&1)"
HELPER_IDENTIFIER="$(printf '%s\n' "$HELPER_SIGN_INFO" | sed -n 's/^Identifier=//p' | head -1)"
if [ "$HELPER_IDENTIFIER" != "dev.linsheng.agenthalo.desktop" ]; then
  echo "helper Identifier ${HELPER_IDENTIFIER:-missing} is not dev.linsheng.agenthalo.desktop" >&2
  exit 1
fi

# Never replace or rewrite the definition of a live helper from this
# standalone registration script. A launchctl bootout/kickstart does not
# promise that the process can finish its shield/grant/relock cleanup. Normal
# agent updates use the helper's atomic prepare_restart handshake first.
if launchctl print "$TARGET/$LABEL" >/dev/null 2>&1; then
  echo "$LABEL is already loaded; refusing an unverified replacement" >&2
  echo "use the running AgentHalo update path, which performs prepare_restart first" >&2
  exit 1
fi

mkdir -p "$(dirname "$PLIST")" "$(dirname "$LOG")" "$SUPPORT_DIR"
chmod 0700 "$SUPPORT_DIR"

cat > "$PLIST" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$HELPER</string>
    <string>--socket</string>
    <string>$SOCKET</string>
    <string>--config</string>
    <string>$CONFIG</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <!-- Restart after a crash, but not after a clean exit: a clean exit is the
       agent stopping the helper on purpose, and relaunching it there would
       fight the operator. A crash is different — the helper owns the shield,
       so one that died may have left the desktop uncovered, and its startup
       scrub relocks. -->
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <!-- Aqua only. Outside a GUI session there are no windows to shield with and
       no desktop to drive. -->
  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>StandardErrorPath</key>
  <string>$LOG</string>
</dict>
</plist>
PLISTEOF

plutil -lint "$PLIST" >/dev/null || { echo "generated plist is not valid" >&2; exit 1; }

# The job was proven unloaded above, so any socket at this point is stale.  It
# must not make a failed bootstrap look healthy.
rm -f "$SOCKET"
launchctl bootstrap "$TARGET" "$PLIST"
launchctl enable "$TARGET/$LABEL"

# `bootstrap` succeeding proves only that launchd accepted the plist.  AMFI or
# Gatekeeper can still reject the executable asynchronously, which otherwise
# leaves a crash-looping job and an installer that falsely reports success.
READY=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if launchctl print "$TARGET/$LABEL" >/dev/null 2>&1 && [ -S "$SOCKET" ]; then
    READY=1
    break
  fi
  sleep 1
done
if [ "$READY" != "1" ]; then
  echo "$LABEL did not create its socket after launchd bootstrap" >&2
  echo "check $LOG and the macOS AMFI/Gatekeeper logs; refusing to report a usable helper" >&2
  exit 1
fi

echo "==> installed $LABEL"
echo "    helper: $HELPER"
echo "    socket: $SOCKET"
echo "    config: $CONFIG"
echo "    log:    $LOG"
echo
echo "    Grant Accessibility and Screen Recording to:"
echo "      $HELPER"
echo "    in System Settings > Privacy & Security. Without them synthetic input"
echo "    and capture silently do nothing, which looks like a broken feature."
