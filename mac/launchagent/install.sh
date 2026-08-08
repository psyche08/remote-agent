#!/usr/bin/env bash
# Installs remote-agent-desktop as a per-user LaunchAgent.
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

LABEL="com.psyche08.remote-agent-desktop"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
SUPPORT_DIR="$HOME/Library/Application Support/remote-agent"
HELPER="$SUPPORT_DIR/bin/remote-agent-desktop"
SOCKET="$SUPPORT_DIR/desktop.sock"
LOG="$HOME/Library/Logs/private-services/remote-agent-desktop.log"
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
  launchctl bootout "$TARGET/$LABEL" 2>/dev/null || true
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
  echo "WARNING: $HELPER has no valid signature; TCC grants will not persist" >&2
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

launchctl bootout "$TARGET/$LABEL" 2>/dev/null || true
launchctl bootstrap "$TARGET" "$PLIST"
launchctl enable "$TARGET/$LABEL"

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
