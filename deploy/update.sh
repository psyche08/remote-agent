#!/usr/bin/env bash
# remote-agent 设备侧更新脚本 —— 由 deploy/publish-release.sh 发布到 relay 的
# assets/release/update.sh;设备上的 auto-updater(internal/autoupdate)在
# manifest 版本与运行版本不一致时下载本脚本 + 对应平台二进制,sha256 校验后
# 执行。保持自包含、纯参数驱动:更新"步骤"随每次发布走 relay 下发,不依赖
# 设备上已装的旧代码。
#
# Usage: update.sh STAGED_BINARY TARGET_PATH [DEVICE_ID]
#
set -euo pipefail

STAGED="${1:?usage: update.sh STAGED_BINARY TARGET_PATH [DEVICE_ID]}"
TARGET="${2:?usage: update.sh STAGED_BINARY TARGET_PATH [DEVICE_ID]}"
DEVICE="${3:-unknown}"
SUPERVISOR="${RA_SUPERVISOR:-${RC_SUPERVISOR:-/opt/private-tunnel/bin/private-services}}"
STATE_DIR="${RA_STATE_DIR:-${RC_STATE_DIR:-/opt/private-tunnel/state/remote-agent}}"
LEGACY_STATE_DIR="${RA_LEGACY_STATE_DIR:-/opt/private-tunnel/state/remote-coding}"
BIN_DIR="${RA_BIN_DIR:-${RC_BIN_DIR:-/opt/private-tunnel/bin}}"
ETC_DIR="${RA_ETC_DIR:-${RC_ETC_DIR:-/opt/private-tunnel/etc}}"
CODESIGN="${RA_CODESIGN:-${RC_CODESIGN:-codesign}}"
EXPECTED_TEAM_ID="${RA_EXPECTED_TEAM_ID:-${RC_EXPECTED_TEAM_ID:-__REMOTE_AGENT_TEAM_ID__}}"
PLATFORM="${RA_PLATFORM:-${RC_PLATFORM:-$(uname -s)}}"

prepare_staged_binary() {
  [ "$PLATFORM" = "Darwin" ] || return 0
  command -v "$CODESIGN" >/dev/null 2>&1 || {
		echo "codesign is required to verify the macOS remote-agent binary" >&2
		return 1
	}
  case "$EXPECTED_TEAM_ID" in
    ""|__REMOTE_AGENT_*)
		echo "expected Developer ID team is missing from update script" >&2
		return 1
		;;
  esac
  verify_signed_binary "$STAGED"
  echo "==> verified signed and notarized staged macOS binary"
}

verify_signed_binary() {
  local path="$1" team
  "$CODESIGN" --verify --strict --verbose=2 "$path" >/dev/null
  team="$("$CODESIGN" -d --verbose=4 "$path" 2>&1 | sed -n 's/^TeamIdentifier=//p' | head -1)"
  [ "$team" = "$EXPECTED_TEAM_ID" ] || {
		echo "Developer ID team mismatch for $path: got ${team:-missing}, want $EXPECTED_TEAM_ID" >&2
		return 1
	}
}

cleanup_legacy_watchdog() {
  rm -f \
    "$BIN_DIR/remote-coding-watchdog" \
    "$BIN_DIR/remote-agent-watchdog" \
    "$ETC_DIR/services.d/remote-coding-watchdog.yaml" \
    "$ETC_DIR/services.d/remote-agent-watchdog.yaml"
  echo "==> removed legacy external remote-agent watchdog; internal watchdog runs in the agent"
}

cleanup_legacy_claude_wrapper() {
  if [ -x "$SUPERVISOR" ]; then
    "$SUPERVISOR" stop remote-coding-claude-wrapper-watch >/dev/null 2>&1 || true
    "$SUPERVISOR" stop remote-agent-claude-wrapper-watch >/dev/null 2>&1 || true
  fi
  rm -f \
    "$ETC_DIR/services.d/remote-coding-claude-wrapper-watch.yaml" \
    "$ETC_DIR/services.d/remote-agent-claude-wrapper-watch.yaml" \
    "$STATE_DIR/data/claude-wrapper-watch.enabled" \
    "$LEGACY_STATE_DIR/data/claude-wrapper-watch.enabled" \
    "$BIN_DIR/claude-wrapper" \
    "$BIN_DIR/install-claude-wrapper" \
    "$BIN_DIR/watch-claude-wrapper"
  echo "==> removed retired Claude Desktop wrapper artifacts"
}

chmod 0755 "$STAGED"
prepare_staged_binary
STAGED_VERSION="$("$STAGED" version 2>/dev/null || echo '{"commit":"unknown"}')"
echo "==> device=$DEVICE staged version: $STAGED_VERSION"

# 原子替换:同目录写临时文件再 mv,正在运行的进程持有旧 inode 不受影响。
mkdir -p "$(dirname "$TARGET")"
cp -f "$STAGED" "$TARGET.new"
chmod 0755 "$TARGET.new"
mv -f "$TARGET.new" "$TARGET"
echo "==> installed $TARGET"
cleanup_legacy_watchdog
cleanup_legacy_claude_wrapper

if [ -x "$SUPERVISOR" ]; then
  "$SUPERVISOR" reload-config >/dev/null 2>&1 || true
  if "$SUPERVISOR" restart remote-agent || "$SUPERVISOR" start remote-agent; then
    "$SUPERVISOR" restart remote-agent-log-upload >/dev/null 2>&1 || true
    echo "==> supervisor restarted remote-agent"
  else
    # A release may reach a device just before its installer migration. Keep
    # that device available; the installer will rename the service/drop-in.
    "$SUPERVISOR" restart remote-coding || "$SUPERVISOR" start remote-coding || true
    "$SUPERVISOR" restart remote-coding-log-upload >/dev/null 2>&1 || true
    echo "==> supervisor restarted legacy remote-coding; run deploy/install.sh to finish identity migration"
  fi
else
  echo "==> supervisor not found at $SUPERVISOR; restart remote-agent manually" >&2
fi
