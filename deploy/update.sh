#!/usr/bin/env bash
# AgentHalo 设备侧更新脚本 —— 由 deploy/publish-release.sh 发布到 relay 的
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
SUPERVISOR="${AGENTHALO_SUPERVISOR:-/opt/private-tunnel/bin/private-services}"
STATE_DIR="${AGENTHALO_STATE_DIR:-/opt/private-tunnel/state/agenthalo}"
ETC_DIR="${AGENTHALO_ETC_DIR:-/opt/private-tunnel/etc}"
CODESIGN="${AGENTHALO_CODESIGN:-codesign}"
EXPECTED_TEAM_ID="${AGENTHALO_EXPECTED_TEAM_ID:-__AGENTHALO_TEAM_ID__}"
EXPECTED_IDENTIFIER="dev.linsheng.agenthalo"
PLATFORM="${AGENTHALO_PLATFORM:-$(uname -s)}"
CLAUDE_SETTINGS="${AGENTHALO_CLAUDE_SETTINGS:-$HOME/.claude/settings.json}"
TURNSTATE_DIR="${AGENTHALO_TURNSTATE_DIR:-$HOME/.claude/agenthalo-turnstate}"
INTERACTION_DIR="${AGENTHALO_INTERACTION_DIR:-$HOME/.claude/agenthalo-interactions}"

prepare_staged_binary() {
  [ "$PLATFORM" = "Darwin" ] || return 0
  command -v "$CODESIGN" >/dev/null 2>&1 || {
		echo "codesign is required to verify the macOS AgentHalo binary" >&2
		return 1
	}
  case "$EXPECTED_TEAM_ID" in
    ""|__AGENTHALO_*)
		echo "expected Developer ID team is missing from update script" >&2
		return 1
		;;
  esac
  verify_signed_binary "$STAGED"
  echo "==> verified signed and notarized staged AgentHalo macOS binary"
}

verify_signed_binary() {
  local path="$1" metadata identifier team
  "$CODESIGN" --verify --strict --verbose=2 "$path" >/dev/null
  metadata="$("$CODESIGN" -d --verbose=4 "$path" 2>&1)"
  identifier="$(printf '%s\n' "$metadata" | sed -n 's/^Identifier=//p' | head -1)"
  team="$(printf '%s\n' "$metadata" | sed -n 's/^TeamIdentifier=//p' | head -1)"
  [ "$identifier" = "$EXPECTED_IDENTIFIER" ] || {
		echo "signing identifier mismatch for $path: got ${identifier:-missing}, want $EXPECTED_IDENTIFIER" >&2
		return 1
	}
  [ "$team" = "$EXPECTED_TEAM_ID" ] || {
		echo "Developer ID team mismatch for $path: got ${team:-missing}, want $EXPECTED_TEAM_ID" >&2
		return 1
	}
  printf '%s\n' "$metadata" | grep -q '^CodeDirectory .*flags=.*(runtime)' || {
		echo "AgentHalo binary is not signed with the hardened runtime: $path" >&2
		return 1
	}
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

# Hooks are part of the Claude Desktop-first control plane, not optional local
# decoration. Reinstall them from the new binary on every update so stale
# remote-agent or removed-app commands cannot own approvals/questions.
install -d -m 0700 "$TURNSTATE_DIR" "$INTERACTION_DIR"
"$TARGET" hook install-turnstate --settings "$CLAUDE_SETTINGS" --binary "$TARGET" \
  --turnstate-dir "$TURNSTATE_DIR" --interaction-dir "$INTERACTION_DIR"
echo "==> refreshed Claude lifecycle and interaction observer hooks"

if [ -x "$SUPERVISOR" ]; then
  # This is a binary-only update. Reloading the global supervisor config can
  # perturb unrelated services and is unnecessary because the active service
  # definition has not changed.
  CURRENT_DROPIN="$ETC_DIR/services.d/agenthalo.yaml"
  if [ -f "$CURRENT_DROPIN" ]; then
    if ! "$SUPERVISOR" restart agenthalo; then
      echo "failed to restart configured AgentHalo service" >&2
      exit 1
    fi
    "$SUPERVISOR" restart agenthalo-log-upload >/dev/null 2>&1 || true
    echo "==> supervisor restarted AgentHalo"
  else
    echo "no configured AgentHalo service drop-in" >&2
    exit 1
  fi
else
  echo "==> supervisor not found at $SUPERVISOR; restart AgentHalo manually" >&2
fi
