#!/usr/bin/env bash
# remote-coding 统一发布脚本 —— 每次发布设备二进制都走这里,取代
# 旧的 publish-static.sh + 设备端 git pull 编译。
#
# 做什么:
#   1. 交叉编译 darwin-arm64 二进制(ldflags 注入 commit + 东八区 built_at)
#   2. 生成 assets/release/manifest.json(commit / built_at / sha256)
#   3. 上传到 relay 的 remotecoding release 目录:
#        assets/release/remote-coding-darwin-arm64
#        assets/release/update.sh
#        assets/release/manifest.json     ←最后上传,设备不会读到半套发布
#   4. 配置 RC_UPDATE_RELAY_URL 的设备每 5 分钟对比 manifest 并自动更新
#
# 完整控制台已嵌入设备二进制并由 /d/<device>/ 提供。relay 根路径只有稳定
# 的设备选择壳，普通 release 不再覆盖它。仅在壳本身确实变化时显式设置:
#   RC_PUBLISH_SHELL=1 remote-agent/deploy/publish-release.sh
#
# `/assets/` 前缀在 relay 静态白名单内(private-tunnel route.go
# rootStaticPrefixes),设备用 agent mTLS 证书即可下载;relay 无需改动。
#
# Usage:
#   remote-agent/deploy/publish-release.sh [ssh_target]
#   RC_RELAY_SSH=user@host remote-agent/deploy/publish-release.sh
#
# ssh_target 必须通过参数或 $RC_RELAY_SSH 显式提供。
# RC_ALLOW_DIRTY=1 跳过脏树检查(版本章会失真,慎用)。
set -euo pipefail

SSH_TARGET="${1:-${RC_RELAY_SSH:-}}"
USER_ID="${RC_RELAY_USER:-remote-coding}"
STATIC_DIR="/var/lib/private-tunnel/static/${USER_ID}/remotecoding"
RELEASE_DIR="${STATIC_DIR}/assets/release"
SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"          # .../remote-agent
REPO_DIR="$SRC_DIR"
GO="${GO:-go}"
GOCACHE="${GOCACHE:-/private/tmp/remote-agent-gocache}"
PLATFORM="darwin-arm64"
BIN_NAME="remote-coding-${PLATFORM}"
NOTARY_TEAM_ID="${NOTARY_TEAM_ID:-}"
NOTARY_APPLE_ID="${NOTARY_APPLE_ID:-}"
NOTARY_PASSWORD="${NOTARY_PASSWORD:-}"

die() { echo "error: $*" >&2; exit 1; }

[[ -n "$SSH_TARGET" ]] || die "relay SSH target required: pass user@host or set RC_RELAY_SSH"
[[ "$(uname -s)" = "Darwin" ]] || die "signed/notarized remote-coding releases must be published on macOS"
command -v security >/dev/null 2>&1 || die "security is required"
command -v codesign >/dev/null 2>&1 || die "codesign is required"
command -v xcrun >/dev/null 2>&1 || die "xcrun is required"
command -v ditto >/dev/null 2>&1 || die "ditto is required"
[[ -n "$NOTARY_TEAM_ID" ]] || die "NOTARY_TEAM_ID is required"
[[ -n "$NOTARY_APPLE_ID" ]] || die "NOTARY_APPLE_ID is required"
[[ -n "$NOTARY_PASSWORD" ]] || die "NOTARY_PASSWORD is required"

SIGN_IDENTITY="$(security find-identity -v -p codesigning | awk -v team="$NOTARY_TEAM_ID" '
  /Developer ID Application:/ && index($0, "(" team ")") { print $2; exit }
')"
[[ -n "$SIGN_IDENTITY" ]] || die "no Developer ID Application certificate found for NOTARY_TEAM_ID=$NOTARY_TEAM_ID"

verify_team_signature() {
  local path="$1" team
  codesign --verify --strict --verbose=2 "$path" >/dev/null
  team="$(codesign -d --verbose=4 "$path" 2>&1 | sed -n 's/^TeamIdentifier=//p' | head -1)"
  [[ "$team" = "$NOTARY_TEAM_ID" ]] || die "signature team mismatch for $(basename "$path"): got ${team:-missing}, want $NOTARY_TEAM_ID"
}

# 版本章 = HEAD;脏树发布会让章与内容脱节(制造"版本永远对不齐"事故),默认拒绝。
if [ "${RC_ALLOW_DIRTY:-0}" != "1" ] && ! git -C "$REPO_DIR" diff --quiet HEAD -- . 2>/dev/null; then
  echo "仓库有未提交改动;先提交,或 RC_ALLOW_DIRTY=1 强行发布" >&2
  exit 1
fi
COMMIT="$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo dev)"
BUILT_AT="$(TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S+08:00)"

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

echo "==> building ${BIN_NAME} commit=${COMMIT} built_at=${BUILT_AT}"
BUILDINFO_PKG="github.com/psyche08/remote-agent/internal/buildinfo"
( cd "$SRC_DIR" && GOCACHE="$GOCACHE" GOOS=darwin GOARCH=arm64 "$GO" build -trimpath \
  -ldflags "-X ${BUILDINFO_PKG}.Version=${COMMIT} -X ${BUILDINFO_PKG}.Commit=${COMMIT} -X ${BUILDINFO_PKG}.BuiltAt=${BUILT_AT}" \
  -o "$OUT/$BIN_NAME" ./cmd/remote-coding )

echo "==> signing Darwin binaries with Developer ID team ${NOTARY_TEAM_ID}"
codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$OUT/$BIN_NAME"
verify_team_signature "$OUT/$BIN_NAME"

mkdir -p "$OUT/notary-payload"
cp "$OUT/$BIN_NAME" "$OUT/notary-payload/"
ditto -c -k --keepParent "$OUT/notary-payload" "$OUT/notary-payload.zip"
echo "==> notarizing signed release payload"
xcrun notarytool submit "$OUT/notary-payload.zip" \
  --apple-id "$NOTARY_APPLE_ID" \
  --password "$NOTARY_PASSWORD" \
  --team-id "$NOTARY_TEAM_ID" \
  --wait
verify_team_signature "$OUT/$BIN_NAME"

sed "s/__REMOTE_CODING_TEAM_ID__/${NOTARY_TEAM_ID}/g" "$SRC_DIR/deploy/update.sh" > "$OUT/update.sh"

sha() { shasum -a 256 "$1" | awk '{print $1}'; }
BIN_SHA="$(sha "$OUT/$BIN_NAME")"
SCRIPT_SHA="$(sha "$OUT/update.sh")"

cat > "$OUT/manifest.json" <<EOF
{
  "commit": "${COMMIT}",
  "built_at": "${BUILT_AT}",
  "signing": {"team_id": "${NOTARY_TEAM_ID}", "notarized": true},
  "binaries": {
    "${PLATFORM}": {"path": "${BIN_NAME}", "sha256": "${BIN_SHA}"}
  },
  "update_script": {"path": "update.sh", "sha256": "${SCRIPT_SHA}"}
}
EOF

if [ "${RC_PUBLISH_DRY_RUN:-0}" = "1" ]; then
  echo "==> dry run;产物在 $OUT:"
  ls -l "$OUT"
  cat "$OUT/manifest.json"
  trap - EXIT
  echo "==> (dry run 保留 $OUT,自行清理)"
  exit 0
fi

put() { # put LOCAL REMOTE_PATH
  echo "==> $1 -> ${SSH_TARGET}:$2"
  ssh -o RemoteCommand=none "$SSH_TARGET" \
    "cat > '$2' && chmod 644 '$2' && echo \"   \$(wc -c < '$2') bytes\"" < "$1"
}

ssh -o RemoteCommand=none "$SSH_TARGET" "mkdir -p '${RELEASE_DIR}'"

# 1) release 工件(manifest 最后)
put "$OUT/$BIN_NAME"  "${RELEASE_DIR}/${BIN_NAME}"
put "$OUT/update.sh"  "${RELEASE_DIR}/update.sh"

# 2) relay 稳定设备壳。普通设备/UI 发布跳过；只有壳本身变化时才显式更新。
if [ "${RC_PUBLISH_SHELL:-0}" = "1" ]; then
  SHELL_VERSION="${RC_SHELL_VERSION:-shell-v1}"
  sed "s/__REMOTE_CODING_SHELL_VERSION__/${SHELL_VERSION}/g" "$SRC_DIR/static/shell.html" > "$OUT/index.html"
  sed "s/__REMOTE_CODING_STATIC_VERSION__/${SHELL_VERSION}/g" "$SRC_DIR/static/sw.js" > "$OUT/sw.js"
  put "$OUT/index.html" "${STATIC_DIR}/index.html"
  put "$OUT/sw.js" "${STATIC_DIR}/sw.js"
  for f in manifest.webmanifest icon-192.png icon-512.png; do
    src="${SRC_DIR}/static/${f}"
    [[ -f "$src" ]] || { echo "missing $src" >&2; exit 1; }
    put "$src" "${STATIC_DIR}/${f}"
  done
else
  echo "==> relay shell unchanged (set RC_PUBLISH_SHELL=1 only when shell changes)"
fi

# 3) manifest 最后落位 —— 设备要么看到旧发布,要么看到完整新发布
put "$OUT/manifest.json" "${RELEASE_DIR}/manifest.json"

echo "==> done. commit=${COMMIT};设备将在 5 分钟内自动更新(或网页端触发 /update)。"
