#!/usr/bin/env bash
# Build, Developer ID sign, and notarize a complete local AgentHalo release.
#
# This is the manual/offline counterpart to deploy/publish-release.sh. It never
# contacts the relay, installs a plug-in, edits authorizationdb, or deploys a
# device. Every retained artifact and build cache stays under the repository's
# ignored build/ directory; the temporary go:embed input is always removed.
#
# Usage:
#   ./build.sh 10
#   ./build.sh --module-version 10
#
# Required environment:
#   NOTARY_TEAM_ID
#   and either:
#     NOTARY_KEYCHAIN_PROFILE
#   or:
#     NOTARY_APPLE_ID
#     NOTARY_PASSWORD
#
# Optional environment:
#   AGENTHALO_SIGN_IDENTITY  exact Developer ID identity or certificate hash;
#                            otherwise select the first identity for the team
#   NOTARY_KEYCHAIN_PATH     explicit Keychain used by notarytool
#   NOTARY_TIMEOUT           bounded --wait duration (default: 30m)
#   AGENTHALO_ALLOW_DIRTY=1  permit a visibly `-dirty` development artifact
# Never let an inherited `bash -x` expose an app-specific notary password in
# variable assignments or notarytool argv. Build progress is printed explicitly.
set +x
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_ROOT="$ROOT/build"
MODULE_VERSION="${AGENTHALO_MODULE_VERSION:-}"
GO="${GO:-go}"

usage() {
  sed -n '2,26p' "$0"
}

die() {
  echo "error: $*" >&2
  exit 1
}

while [ $# -gt 0 ]; do
  case "$1" in
    --module-version)
      shift
      MODULE_VERSION="${1:-}"
      [ -n "$MODULE_VERSION" ] || die "--module-version requires an integer"
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      if [[ "$1" =~ ^[1-9][0-9]*$ ]]; then
        [ -z "$MODULE_VERSION" ] || die "module version was provided more than once"
        MODULE_VERSION="$1"
      else
        die "unknown option: $1"
      fi
      ;;
  esac
  shift
done

[[ "$MODULE_VERSION" =~ ^[1-9][0-9]*$ ]] || \
  die "module version is required and must be a positive integer"
[ "$(uname -s)" = "Darwin" ] || die "formal AgentHalo builds require macOS"

for tool in git "$GO" swift clang security codesign xcrun ditto shasum python3 lipo cmp diff xargs; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

NOTARY_TEAM_ID="${NOTARY_TEAM_ID:-}"
NOTARY_KEYCHAIN_PROFILE="${NOTARY_KEYCHAIN_PROFILE:-}"
NOTARY_KEYCHAIN_PATH="${NOTARY_KEYCHAIN_PATH:-}"
NOTARY_APPLE_ID="${NOTARY_APPLE_ID:-}"
NOTARY_PASSWORD="${NOTARY_PASSWORD:-}"
NOTARY_TIMEOUT="${NOTARY_TIMEOUT:-30m}"
[ -n "$NOTARY_TEAM_ID" ] || die "NOTARY_TEAM_ID is required"
[[ "$NOTARY_TIMEOUT" =~ ^[1-9][0-9]*(s|m|h)?$ ]] || \
  die "NOTARY_TIMEOUT must be a positive duration such as 30m"

NOTARY_AUTH_ARGS=()
if [ -n "$NOTARY_KEYCHAIN_PROFILE" ]; then
  NOTARY_AUTH_ARGS=(--keychain-profile "$NOTARY_KEYCHAIN_PROFILE")
else
  [ -n "$NOTARY_APPLE_ID" ] || \
    die "set NOTARY_KEYCHAIN_PROFILE or NOTARY_APPLE_ID"
  [ -n "$NOTARY_PASSWORD" ] || \
    die "NOTARY_PASSWORD is required when no keychain profile is configured"
  NOTARY_AUTH_ARGS=(
    --apple-id "$NOTARY_APPLE_ID"
    --password "$NOTARY_PASSWORD"
    --team-id "$NOTARY_TEAM_ID"
  )
fi
if [ -n "$NOTARY_KEYCHAIN_PATH" ]; then
  NOTARY_AUTH_ARGS+=(--keychain "$NOTARY_KEYCHAIN_PATH")
fi

if ! DIRTY_STATUS="$(git -C "$ROOT" status --porcelain --untracked-files=normal -- . 2>/dev/null)"; then
  die "could not inspect repository status"
fi
SOURCE_DIRTY=false
if [ -n "$DIRTY_STATUS" ]; then
  if [ "${AGENTHALO_ALLOW_DIRTY:-0}" != "1" ]; then
    echo "$DIRTY_STATUS" >&2
    die "repository has tracked or untracked changes; commit them first"
  fi
  SOURCE_DIRTY=true
fi

COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)"
COMMIT_LABEL="$COMMIT"
if [ "$SOURCE_DIRTY" = true ]; then
  COMMIT_LABEL="${COMMIT}-dirty"
fi
BUILT_AT="$(TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S+08:00)"

if [ -L "$BUILD_ROOT" ]; then
  die "refusing symlink build directory: $BUILD_ROOT"
fi
if [ -e "$BUILD_ROOT" ] && [ ! -d "$BUILD_ROOT" ]; then
  die "build path exists and is not a directory: $BUILD_ROOT"
fi
mkdir -p "$BUILD_ROOT"

FINAL_DIR="$BUILD_ROOT/AgentHalo.${MODULE_VERSION}-${COMMIT_LABEL}"
if [ -e "$FINAL_DIR" ] || [ -L "$FINAL_DIR" ]; then
  die "output already exists: $FINAL_DIR"
fi
STAGE="$(mktemp -d "$BUILD_ROOT/.AgentHalo.${MODULE_VERSION}-${COMMIT_LABEL}.XXXXXX")"
WORK="$STAGE/.work"
mkdir -p "$WORK"

DESKTOP_ASSET="$ROOT/internal/desktopasset/assets/agenthalo-desktop"
ASSET_STAGED=0
BUILD_SUCCEEDED=0
cleanup() {
  if [ "$ASSET_STAGED" = "1" ]; then
    rm -f -- "$DESKTOP_ASSET"
  fi
  if [ "$BUILD_SUCCEEDED" != "1" ] && [ -n "${STAGE:-}" ] && [ -d "$STAGE" ]; then
    case "$STAGE" in
      "$BUILD_ROOT"/.AgentHalo.*)
        rm -rf -- "$STAGE"
        ;;
    esac
  fi
}
trap cleanup EXIT

SIGN_IDENTITY="${AGENTHALO_SIGN_IDENTITY:-}"
if [ -z "$SIGN_IDENTITY" ]; then
  SIGN_IDENTITY="$(security find-identity -v -p codesigning | awk -v team="$NOTARY_TEAM_ID" '
    /Developer ID Application:/ && index($0, "(" team ")") { print $2; exit }
  ')"
fi
[ -n "$SIGN_IDENTITY" ] || \
  die "no Developer ID Application identity found for team $NOTARY_TEAM_ID"
[ "$SIGN_IDENTITY" != "-" ] || die "ad-hoc signing is forbidden"

verify_signature() {
  local path="$1" expected_identifier="$2" identifier team timestamp metadata
  codesign --verify --strict --verbose=2 "$path" >/dev/null
  metadata="$(codesign -d --verbose=4 "$path" 2>&1)"
  identifier="$(sed -n 's/^Identifier=//p' <<<"$metadata" | head -1)"
  team="$(sed -n 's/^TeamIdentifier=//p' <<<"$metadata" | head -1)"
  timestamp="$(sed -n 's/^Timestamp=//p' <<<"$metadata" | head -1)"
  [ "$identifier" = "$expected_identifier" ] || \
    die "identifier mismatch for $path: ${identifier:-missing}"
  [ "$team" = "$NOTARY_TEAM_ID" ] || \
    die "team mismatch for $path: ${team:-missing}"
  grep -Eq '^CodeDirectory .*flags=.*\(runtime\)' <<<"$metadata" || \
    die "hardened runtime is missing from $path"
  [ -n "$timestamp" ] || die "trusted signing timestamp is missing from $path"
  grep -Fq 'Authority=Developer ID Application:' <<<"$metadata" || \
    die "Developer ID Application authority is missing from $path"
}

verify_architectures() {
  local path="$1" expected="$2" actual
  actual="$(lipo -archs "$path" | tr ' ' '\n' | LC_ALL=C sort | xargs)"
  [ "$actual" = "$expected" ] || \
    die "architecture mismatch for $path: got ${actual:-missing}, want $expected"
}

sha256() {
  shasum -a 256 "$1" | awk '{print $1}'
}

cdhash_for_arch() {
  local path="$1" arch="$2" cdhash
  cdhash="$(codesign -d --arch "$arch" --verbose=4 "$path" 2>&1 \
    | sed -n 's/^CDHash=//p' | head -1)"
  [[ "$cdhash" =~ ^[0-9a-fA-F]{40}$ ]] || \
    die "could not read the $arch CodeDirectory hash for $path"
  printf '%s\n' "$(tr '[:upper:]' '[:lower:]' <<<"$cdhash")"
}

echo "==> building AgentHalo.${MODULE_VERSION} from ${COMMIT_LABEL}"
echo "==> output will be $FINAL_DIR"

echo "==> building and signing agenthalo-desktop"
AGENTHALO_SWIFT_SCRATCH_PATH="$WORK/swift" \
AGENTHALO_SIGN_IDENTITY="$SIGN_IDENTITY" \
AGENTHALO_SIGN_TEAM_ID="$NOTARY_TEAM_ID" \
  bash "$ROOT/mac/RemoteAgentDesktop/build.sh" \
    --out "$STAGE/agenthalo-desktop"
verify_signature "$STAGE/agenthalo-desktop" dev.linsheng.agenthalo.desktop
verify_architectures "$STAGE/agenthalo-desktop" arm64

echo "==> building and signing AgentHaloLockedUse.bundle"
AGENTHALO_PLUGIN_BUILD_DIR="$STAGE" \
AGENTHALO_PLUGIN_SIGN_IDENTITY="$SIGN_IDENTITY" \
  bash "$ROOT/mac/authorization-plugin/build.sh"
verify_signature "$STAGE/AgentHaloLockedUse.bundle" \
  dev.linsheng.agenthalo.locked-use.plugin
verify_architectures \
  "$STAGE/AgentHaloLockedUse.bundle/Contents/MacOS/AgentHaloLockedUse" \
  "arm64 x86_64"

if [ -e "$DESKTOP_ASSET" ] || [ -L "$DESKTOP_ASSET" ]; then
  die "refusing to overwrite stale embedded helper: $DESKTOP_ASSET"
fi
ASSET_STAGED=1
cp "$STAGE/agenthalo-desktop" "$DESKTOP_ASSET"

echo "==> building and signing agenthalo-darwin-arm64"
BUILDINFO_PKG="github.com/psyche08/remote-agent/internal/buildinfo"
(
  cd "$ROOT"
  GOCACHE="$WORK/go-cache" GOOS=darwin GOARCH=arm64 "$GO" build -trimpath \
    -ldflags "-X ${BUILDINFO_PKG}.Version=AgentHalo.${MODULE_VERSION} -X ${BUILDINFO_PKG}.Commit=${COMMIT_LABEL} -X ${BUILDINFO_PKG}.BuiltAt=${BUILT_AT}" \
    -o "$STAGE/agenthalo-darwin-arm64" ./cmd/agenthalo
)
rm -f -- "$DESKTOP_ASSET"
ASSET_STAGED=0
codesign --force --identifier dev.linsheng.agenthalo --options runtime --timestamp \
  --sign "$SIGN_IDENTITY" "$STAGE/agenthalo-darwin-arm64"
verify_signature "$STAGE/agenthalo-darwin-arm64" dev.linsheng.agenthalo
verify_architectures "$STAGE/agenthalo-darwin-arm64" arm64

python3 - "$STAGE/agenthalo-darwin-arm64" "$MODULE_VERSION" "$COMMIT_LABEL" <<'PY'
import json, subprocess, sys
binary, version, commit = sys.argv[1:]
info = json.loads(subprocess.check_output([binary, "version"], text=True))
if info.get("version") != "AgentHalo." + version:
    raise SystemExit("built main binary contains the wrong module version")
if info.get("commit") != commit:
    raise SystemExit("built main binary contains the wrong commit label")
PY

EMBEDDED_CHECK="$WORK/materialized-agenthalo-desktop"
"$STAGE/agenthalo-darwin-arm64" desktop install \
  --path "$EMBEDDED_CHECK" >/dev/null
cmp -s "$EMBEDDED_CHECK" "$STAGE/agenthalo-desktop" || \
  die "main binary does not embed the exact signed desktop helper"
verify_signature "$EMBEDDED_CHECK" dev.linsheng.agenthalo.desktop
rm -f -- "$EMBEDDED_CHECK"

# The helper is opaque go:embed data inside the main executable, so both Mach-O
# files must also be top-level notarization inputs. The Authorization Plug-in is
# included as a signed top-level bundle in the same submission. Bare Mach-O and
# Authorization Plug-in bundles are not stapled here; Accepted notary results
# and the full Apple log are retained beside the artifacts instead.
mkdir -p "$STAGE/notary-payload"
cp "$STAGE/agenthalo-darwin-arm64" "$STAGE/notary-payload/"
cp "$STAGE/agenthalo-desktop" "$STAGE/notary-payload/"
ditto -c -k --keepParent "$STAGE/AgentHaloLockedUse.bundle" \
  "$STAGE/AgentHaloLockedUse.bundle.zip"
ditto -x -k "$STAGE/AgentHaloLockedUse.bundle.zip" \
  "$STAGE/notary-payload"
ditto -c -k --keepParent "$STAGE/notary-payload" \
  "$STAGE/notary-payload.zip"
NOTARY_SHA="$(sha256 "$STAGE/notary-payload.zip")"
MAIN_CDHASH="$(cdhash_for_arch "$STAGE/agenthalo-darwin-arm64" arm64)"
HELPER_CDHASH="$(cdhash_for_arch "$STAGE/agenthalo-desktop" arm64)"
PLUGIN_ARM64_CDHASH="$(cdhash_for_arch "$STAGE/AgentHaloLockedUse.bundle" arm64)"
PLUGIN_X86_CDHASH="$(cdhash_for_arch "$STAGE/AgentHaloLockedUse.bundle" x86_64)"

echo "==> submitting main, helper, and plug-in to Apple notary service"
if ! xcrun notarytool submit "${NOTARY_AUTH_ARGS[@]}" \
  --wait --timeout "$NOTARY_TIMEOUT" --output-format json \
  "$STAGE/notary-payload.zip" \
  > "$STAGE/notarization.json"; then
  die "notarytool submission failed; inspect $STAGE/notarization.json"
fi

read -r SUBMISSION_ID NOTARY_STATUS < <(python3 - "$STAGE/notarization.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    result = json.load(f)
print(result.get("id", ""), result.get("status", ""))
PY
)
[ -n "$SUBMISSION_ID" ] || die "notarytool returned no submission id"
[ "$NOTARY_STATUS" = "Accepted" ] || \
  die "Apple notarization was not accepted: ${NOTARY_STATUS:-unknown}"

xcrun notarytool log "${NOTARY_AUTH_ARGS[@]}" \
  "$SUBMISSION_ID" "$STAGE/notarization-log.json"
python3 - \
  "$STAGE/notarization-log.json" \
  "$NOTARY_SHA" \
  "$MAIN_CDHASH" \
  "$HELPER_CDHASH" \
  "$PLUGIN_ARM64_CDHASH" \
  "$PLUGIN_X86_CDHASH" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    log = json.load(f)
if log.get("status") != "Accepted":
    raise SystemExit("notarization log status is not Accepted")
if log.get("issues") not in (None, []):
    raise SystemExit("notarization log contains issues")
if log.get("archiveFilename") != "notary-payload.zip":
    raise SystemExit("notarization log names an unexpected archive")
if log.get("sha256") != sys.argv[2]:
    raise SystemExit("Apple notarized archive hash does not match local payload")

prefix = "notary-payload.zip/notary-payload/"
expected = {
    (prefix + "agenthalo-darwin-arm64", "arm64"): sys.argv[3],
    (prefix + "agenthalo-desktop", "arm64"): sys.argv[4],
    (prefix + "AgentHaloLockedUse.bundle/Contents/MacOS/AgentHaloLockedUse", "arm64"): sys.argv[5],
    (prefix + "AgentHaloLockedUse.bundle/Contents/MacOS/AgentHaloLockedUse", "x86_64"): sys.argv[6],
}
observed = {}
for ticket in log.get("ticketContents") or []:
    key = (ticket.get("path"), ticket.get("arch"))
    if key in observed:
        raise SystemExit("notarization log contains a duplicate code ticket")
    if ticket.get("digestAlgorithm") != "SHA-256":
        raise SystemExit("notarization log contains a non-SHA-256 code ticket")
    observed[key] = str(ticket.get("cdhash", "")).lower()
for key, cdhash in expected.items():
    if observed.get(key) != cdhash:
        raise SystemExit("notarization log is missing an exact AgentHalo code ticket: " + repr(key))
PY

# Re-extract the exact outer ZIP whose SHA Apple accepted, then prove that its
# three code objects are byte-for-byte the deliverables retained in build/.
# This closes the gap between "a payload was notarized" and the final manifest
# labeling a different same-Team signed file as notarized.
mkdir -p "$WORK/accepted" "$WORK/plugin-deliverable"
ditto -x -k "$STAGE/notary-payload.zip" "$WORK/accepted"
ditto -x -k "$STAGE/AgentHaloLockedUse.bundle.zip" \
  "$WORK/plugin-deliverable"
ACCEPTED_ROOT="$WORK/accepted/notary-payload"
cmp -s "$ACCEPTED_ROOT/agenthalo-darwin-arm64" \
  "$STAGE/agenthalo-darwin-arm64" || \
  die "notarized main binary differs from the final deliverable"
cmp -s "$ACCEPTED_ROOT/agenthalo-desktop" \
  "$STAGE/agenthalo-desktop" || \
  die "notarized desktop helper differs from the final deliverable"
diff -qr "$ACCEPTED_ROOT/AgentHaloLockedUse.bundle" \
  "$WORK/plugin-deliverable/AgentHaloLockedUse.bundle" >/dev/null || \
  die "notarized Authorization Plug-in differs from the final deliverable"

verify_signature "$STAGE/agenthalo-darwin-arm64" dev.linsheng.agenthalo
verify_signature "$STAGE/agenthalo-desktop" dev.linsheng.agenthalo.desktop
verify_signature "$STAGE/AgentHaloLockedUse.bundle" \
  dev.linsheng.agenthalo.locked-use.plugin

sed "s/__AGENTHALO_TEAM_ID__/${NOTARY_TEAM_ID}/g" \
  "$ROOT/deploy/update.sh" > "$STAGE/update.sh"
chmod 0755 "$STAGE/update.sh"

MAIN_SHA="$(sha256 "$STAGE/agenthalo-darwin-arm64")"
HELPER_SHA="$(sha256 "$STAGE/agenthalo-desktop")"
PLUGIN_SHA="$(sha256 "$STAGE/AgentHaloLockedUse.bundle.zip")"
UPDATE_SHA="$(sha256 "$STAGE/update.sh")"

cat > "$STAGE/manifest.json" <<EOF
{
  "module_version": ${MODULE_VERSION},
  "commit": "${COMMIT_LABEL}",
  "source_dirty": ${SOURCE_DIRTY},
  "built_at": "${BUILT_AT}",
  "signing": {
    "team_id": "${NOTARY_TEAM_ID}",
    "notarized": true,
    "submission_id": "${SUBMISSION_ID}"
  },
  "binaries": {
    "darwin-arm64": {
      "path": "agenthalo-darwin-arm64",
      "identifier": "dev.linsheng.agenthalo",
      "sha256": "${MAIN_SHA}"
    },
    "desktop-helper": {
      "path": "agenthalo-desktop",
      "identifier": "dev.linsheng.agenthalo.desktop",
      "sha256": "${HELPER_SHA}"
    }
  },
  "authorization_plugin": {
    "path": "AgentHaloLockedUse.bundle.zip",
    "identifier": "dev.linsheng.agenthalo.locked-use.plugin",
    "sha256": "${PLUGIN_SHA}"
  },
  "update_script": {"path": "update.sh", "sha256": "${UPDATE_SHA}"},
  "notary_payload": {"path": "notary-payload.zip", "sha256": "${NOTARY_SHA}"}
}
EOF

(
  cd "$STAGE"
  shasum -a 256 \
    agenthalo-darwin-arm64 \
    agenthalo-desktop \
    AgentHaloLockedUse.bundle.zip \
    update.sh \
    notary-payload.zip \
    notarization.json \
    notarization-log.json \
    manifest.json > SHA256SUMS
)

rm -rf -- "$WORK"
mv "$STAGE" "$FINAL_DIR"
STAGE=""
BUILD_SUCCEEDED=1

echo "==> complete"
echo "    artifacts: $FINAL_DIR"
echo "    notary submission: $SUBMISSION_ID (Accepted)"
echo "    manifest: $FINAL_DIR/manifest.json"
