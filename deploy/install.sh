#!/usr/bin/env bash
# Idempotent installer for AgentHalo on one Mac.
#
# It registers the agent with the private-services supervisor via a DROP-IN file
# (services.d/agenthalo.yaml) — it NEVER edits or replaces the shared
# services.yaml. Re-running is safe.
#
# Auth model (Plan A, the only model): the agent listens on a 0700 Unix domain
# socket; access is gated by the socket's filesystem permissions plus the relay's
# mTLS. There is no app-layer bearer token.
#
# What it does:
#   1. build Go backend binary
#   2. config.json from config.example.json     (only if missing — never clobbers)
#   3. the Unix-socket dir                      (run dir for the UDS)
#   4. the supervisor drop-in                   (services.d/agenthalo.yaml)
#   5. reload-config + start agenthalo         (never the container agent)
#
# Usage:
#   ./install.sh DEVICE_ID [options]
#
# macOS signing (required):
#   AGENTHALO_EXPECTED_TEAM_ID=ABCDE12345 \
#   AGENTHALO_SIGN_IDENTITY="Developer ID Application: ..." \
#     ./install.sh DEVICE_ID [options]
#
# With AGENTHALO_SKIP_BUILD=1, bin/agenthalo must already have the exact
# dev.linsheng.agenthalo identifier, the expected Team ID and hardened runtime.
#     --devices a,b,c        fleet device ids for the unified console (default: DEVICE_ID)
#     --uds PATH             socket path (default: /opt/private-tunnel/state/agenthalo/sockets/backend.sock)
#     --agent-config PATH    retired; ingress is owned by private-edge profiles
#     --etc DIR              supervisor config dir (default: /opt/private-tunnel/etc)
#     --update-relay-url URL persist AGENTHALO_UPDATE_RELAY_URL for manifest polling
#     --update-cert-dir DIR  persist AGENTHALO_UPDATE_CERT_DIR for updater mTLS certs
#     --log-user USER        private-tunnel user id for log upload cert discovery
#     --log-cert-dir DIR     client certificate dir for log upload
set -euo pipefail

find_tool() {
  name="$1"; shift
  if command -v "$name" >/dev/null 2>&1; then
    command -v "$name"
    return 0
  fi
  for p in "$@"; do
    if [ -x "$p" ]; then
      echo "$p"
      return 0
    fi
  done
  echo "$name"
}

ensure_runtime_dir() {
  local dir="$1"
  if [ -d "$dir" ] || mkdir -p "$dir" 2>/dev/null; then
    return 0
  fi
  command -v sudo >/dev/null 2>&1 || {
    echo "cannot create runtime directory $dir (sudo unavailable)" >&2
    return 1
  }
  sudo -n install -d -o "$(id -un)" -g staff -m 0755 "$dir"
}

yaml_quote() {
  printf "'"
  printf '%s' "$1" | sed "s/'/''/g"
  printf "'"
}

DEVICE_ID="${1:?usage: install.sh DEVICE_ID [--devices a,b] [--uds path] [--agent-config path]}"
shift || true

REPO_AGENTHALO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # source checkout
ETC_DIR="${AGENTHALO_ETC_DIR:-/opt/private-tunnel/etc}"
LIBEXEC_DIR="${AGENTHALO_LIBEXEC_DIR:-/opt/private-tunnel/libexec/agenthalo}"
RUNTIME_BIN="$LIBEXEC_DIR/agenthalo"
STATE_DIR="${AGENTHALO_STATE_DIR:-/opt/private-tunnel/state/agenthalo}"
UDS="$STATE_DIR/sockets/backend.sock"
DEVICES="$DEVICE_ID"
PORT=8765
LOG_UPLOAD=1
LOG_USER="${AGENTHALO_LOG_UPLOAD_USER:-}"
LOG_CERT_DIR="${AGENTHALO_LOG_UPLOAD_CERT_DIR:-}"
LOG_RELAY_URL="${AGENTHALO_LOG_UPLOAD_RELAY_URL:-}"
UPDATE_RELAY_URL="${AGENTHALO_UPDATE_RELAY_URL:-}"
UPDATE_CERT_DIR="${AGENTHALO_UPDATE_CERT_DIR:-}"
LOG_NAMESPACE="${AGENTHALO_LOG_UPLOAD_NAMESPACE:-agenthalo}"
LOG_INTERVAL="${AGENTHALO_LOG_UPLOAD_INTERVAL:-60s}"
LOG_MAX_CHUNK="${AGENTHALO_LOG_UPLOAD_MAX_CHUNK:-1048576}"
# launchd may run with a minimal PATH; keep versioned Homebrew fallbacks.
PY="$(find_tool python3 /opt/homebrew/bin/python3 /usr/local/bin/python3 /usr/bin/python3)"
GO="$(find_tool go /opt/homebrew/bin/go /opt/homebrew/opt/go/bin/go /opt/homebrew/opt/go@1.25/bin/go /opt/homebrew/opt/go@1.24/bin/go /usr/local/bin/go)"
SUPERVISOR="${AGENTHALO_SUPERVISOR:-/opt/private-tunnel/bin/private-services}"
PLATFORM="${AGENTHALO_PLATFORM:-$(uname -s)}"
CODESIGN="${AGENTHALO_CODESIGN:-codesign}"
SIGN_IDENTITY="${AGENTHALO_SIGN_IDENTITY:-}"
EXPECTED_TEAM_ID="${AGENTHALO_EXPECTED_TEAM_ID:-}"
EXPECTED_IDENTIFIER="dev.linsheng.agenthalo"

while [ $# -gt 0 ]; do
  case "$1" in
    --devices) DEVICES="$2"; shift 2 ;;
    --uds) UDS="$2"; shift 2 ;;
    --agent-config) echo "--agent-config is retired; deploy/update private-edge instead" >&2; exit 2 ;;
    --etc) ETC_DIR="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --update-relay-url) UPDATE_RELAY_URL="$2"; shift 2 ;;
    --update-cert-dir) UPDATE_CERT_DIR="$2"; shift 2 ;;
    --no-log-upload) LOG_UPLOAD=0; shift ;;
    --log-user) LOG_USER="$2"; shift 2 ;;
    --log-cert-dir) LOG_CERT_DIR="$2"; shift 2 ;;
    --log-relay-url) LOG_RELAY_URL="$2"; shift 2 ;;
    --log-namespace) LOG_NAMESPACE="$2"; shift 2 ;;
    --log-interval) LOG_INTERVAL="$2"; shift 2 ;;
    --log-max-chunk) LOG_MAX_CHUNK="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

if [ "$LOG_UPLOAD" = "1" ] && [ -z "$LOG_RELAY_URL" ]; then
  echo "log upload requires --log-relay-url URL, AGENTHALO_LOG_UPLOAD_RELAY_URL, or --no-log-upload" >&2
  exit 2
fi

echo "==> AgentHalo install: device=$DEVICE_ID repo=$REPO_AGENTHALO"
if [ "${AGENTHALO_SKIP_BUILD:-0}" != "1" ] && [ ! -x "$GO" ]; then
  echo "go not found; install Go or set PATH before running install.sh" >&2
  exit 127
fi

VERSION_BASE="$(tr -d '[:space:]' <"$REPO_AGENTHALO/VERSION")"
VERSION_STATE="${AGENTHALO_VERSION_STATE:-$STATE_DIR/deployment-version}"
VERSION_CURRENT="$VERSION_BASE"
if [ -r "$VERSION_STATE" ]; then VERSION_CURRENT="$(tr -d '[:space:]' <"$VERSION_STATE")"; fi
case "$VERSION_BASE:$VERSION_CURRENT:${AGENTHALO_VERSION_INCREMENT:-1}" in
  *[!0-9:]*|:*|*:) echo "invalid AgentHalo deployment version" >&2; exit 1 ;;
esac
VERSION_INCREMENT="${AGENTHALO_VERSION_INCREMENT:-1}"
[ "$VERSION_INCREMENT" -gt 0 ] || { echo "AGENTHALO_VERSION_INCREMENT must be positive" >&2; exit 1; }
MODULE_VERSION=$((VERSION_CURRENT + VERSION_INCREMENT))
echo "==> AgentHalo deployment version: $VERSION_CURRENT -> $MODULE_VERSION"
if [ ! -x "$PY" ]; then
  echo "python3 not found; install Python 3 or set PATH before running install.sh" >&2
  exit 127
fi

verify_agenthalo_signature() {
  local path="$1" metadata identifier team
  "$CODESIGN" --verify --strict --verbose=2 "$path" >/dev/null
  metadata="$("$CODESIGN" -d --verbose=4 "$path" 2>&1)"
  identifier="$(printf '%s\n' "$metadata" | sed -n 's/^Identifier=//p' | head -1)"
  team="$(printf '%s\n' "$metadata" | sed -n 's/^TeamIdentifier=//p' | head -1)"
  [ "$identifier" = "$EXPECTED_IDENTIFIER" ] || {
    echo "AgentHalo signing identifier mismatch for $path: got ${identifier:-missing}, want $EXPECTED_IDENTIFIER" >&2
    return 1
  }
  [ "$team" = "$EXPECTED_TEAM_ID" ] || {
    echo "AgentHalo signing team mismatch for $path: got ${team:-missing}, want $EXPECTED_TEAM_ID" >&2
    return 1
  }
  printf '%s\n' "$metadata" | grep -q '^CodeDirectory .*flags=.*(runtime)' || {
    echo "AgentHalo binary is not signed with the hardened runtime: $path" >&2
    return 1
  }
}

if [ "$PLATFORM" = "Darwin" ]; then
  command -v "$CODESIGN" >/dev/null 2>&1 || {
    echo "codesign is required for an AgentHalo macOS install" >&2
    exit 1
  }
  [ -n "$EXPECTED_TEAM_ID" ] || {
    echo "AGENTHALO_EXPECTED_TEAM_ID is required for an AgentHalo macOS install" >&2
    exit 1
  }
  if [ "${AGENTHALO_SKIP_BUILD:-0}" != "1" ]; then
    case "$SIGN_IDENTITY" in
      ""|-) echo "AGENTHALO_SIGN_IDENTITY must be a non-ad-hoc signing identity for a local macOS build" >&2; exit 1 ;;
    esac
  fi
fi

# Validate and prepare config before stopping services or replacing installed
# binaries. A bad explicit source must not interrupt a healthy AgentHalo.
CFG="${AGENTHALO_CONFIG:-$ETC_DIR/agenthalo/config.json}"
SOURCE_CFG="${AGENTHALO_CONFIG_SOURCE:-}"
validate_agenthalo_config() {
  "$PY" - "$1" <<'PYEOF'
import json, sys

path = sys.argv[1]
with open(path) as f:
    config = json.load(f)

old_tokens = ("remote-agent", "remote-coding", "remotecoding", "com.psyche08.remote-agent")
violations = []

def check(location, value):
    if not isinstance(value, str):
        return
    lowered = value.lower()
    for token in old_tokens:
        if token in lowered:
            violations.append(f"{location}: old product token {token!r}")
            return

check("$.uds", config.get("uds"))
check("$.state_dir", config.get("state_dir"))
computer_use = config.get("computer_use") or {}
if isinstance(computer_use, dict):
    check("$.computer_use.helper_socket", computer_use.get("helper_socket"))
    locked_use = computer_use.get("locked_use") or {}
    if isinstance(locked_use, dict):
        check("$.computer_use.locked_use.grant_dir", locked_use.get("grant_dir"))
        if "signing_key_path" in locked_use:
            violations.append("$.computer_use.locked_use.signing_key_path: removed plaintext signing-key field")
providers = config.get("providers") or {}
if isinstance(providers, dict):
    for provider_id, provider in providers.items():
        if isinstance(provider, dict):
            check(f"$.providers.{provider_id}.turnstate_dir", provider.get("turnstate_dir"))
            check(f"$.providers.{provider_id}.interaction_dir", provider.get("interaction_dir"))

if violations:
    print("refusing non-AgentHalo config:\n  " + "\n  ".join(violations), file=sys.stderr)
    raise SystemExit(1)
PYEOF
}
mkdir -p "$(dirname "$CFG")"
if [ ! -f "$CFG" ] && [ -n "$SOURCE_CFG" ]; then
  [ -f "$SOURCE_CFG" ] || {
    echo "AGENTHALO_CONFIG_SOURCE does not exist: $SOURCE_CFG" >&2
    exit 1
  }
  validate_agenthalo_config "$SOURCE_CFG"
  cp -p "$SOURCE_CFG" "$CFG"
  echo "==> installed explicit AgentHalo config $SOURCE_CFG -> $CFG"
fi
if [ -f "$CFG" ]; then
  echo "==> config.json exists — preserving user settings"
else
  echo "==> writing config.json (device_id=$DEVICE_ID, uds=$UDS)"
  DEVICE_ID="$DEVICE_ID" DEVICES="$DEVICES" UDS="$UDS" STATE_DIR="$STATE_DIR" PORT="$PORT" \
  "$PY" - "$CFG" "$REPO_AGENTHALO/config.example.json" <<'PYEOF'
import json, os, sys
out, example = sys.argv[1], sys.argv[2]
d = json.load(open(example))
d["device_id"] = os.environ["DEVICE_ID"]
d["devices"] = [x for x in os.environ["DEVICES"].split(",") if x]
d["port"] = int(os.environ["PORT"])
d["uds"] = os.environ["UDS"]
d["state_dir"] = os.environ["STATE_DIR"]
json.dump(d, open(out, "w"), ensure_ascii=False, indent=2)
PYEOF
fi
validate_agenthalo_config "$CFG"
# The configuration can later hold push or provider credentials.  A fresh
# AgentHalo install must not leave it readable by other local accounts.
chmod 0600 "$CFG"

# 1) Go backend -------------------------------------------------------------
if [ "${AGENTHALO_SKIP_BUILD:-0}" = "1" ]; then
  [ -x "$REPO_AGENTHALO/bin/agenthalo" ] || { echo "AGENTHALO_SKIP_BUILD requires bin/agenthalo" >&2; exit 1; }
  echo "==> using prebuilt $REPO_AGENTHALO/bin/agenthalo"
else
  echo "==> building Go backend"
  BUILD_COMMIT="$(git -C "$REPO_AGENTHALO" rev-parse --short HEAD 2>/dev/null || echo dev)"
  BUILD_AT="$(TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S+08:00)"
  BUILDINFO_PKG="github.com/psyche08/remote-agent/internal/buildinfo"
  ( cd "$REPO_AGENTHALO" && GOCACHE="${GOCACHE:-/private/tmp/agenthalo-gocache}" "$GO" build -trimpath \
    -ldflags "-X ${BUILDINFO_PKG}.Version=AgentHalo.${MODULE_VERSION} -X ${BUILDINFO_PKG}.Commit=${BUILD_COMMIT} -X ${BUILDINFO_PKG}.BuiltAt=${BUILD_AT}" \
    -o bin/agenthalo ./cmd/agenthalo )
  echo "==> built $REPO_AGENTHALO/bin/agenthalo"
  if [ "$PLATFORM" = "Darwin" ]; then
    "$CODESIGN" --force --identifier "$EXPECTED_IDENTIFIER" --options runtime --timestamp \
      --sign "$SIGN_IDENTITY" "$REPO_AGENTHALO/bin/agenthalo"
  fi
fi
if [ "$PLATFORM" = "Darwin" ]; then
  verify_agenthalo_signature "$REPO_AGENTHALO/bin/agenthalo"
  echo "==> verified AgentHalo signature identifier=$EXPECTED_IDENTIFIER team=$EXPECTED_TEAM_ID"
fi

# Install an immutable runtime copy. Active supervisor drop-ins must never
# reference a Git checkout or a temporary deployment worktree.
ensure_runtime_dir "$LIBEXEC_DIR"
install -m 0755 "$REPO_AGENTHALO/bin/agenthalo" "$RUNTIME_BIN.new"
mv -f "$RUNTIME_BIN.new" "$RUNTIME_BIN"
echo "==> installed runtime binary $RUNTIME_BIN"

# The native Swift workers are looked up relative to the service's cwd, which is
# $LIBEXEC_DIR. Install them alongside the binary so the OCR worker resolves at
# runtime instead of silently reporting "not available".
if [ -d "$REPO_AGENTHALO/scripts" ]; then
  ensure_runtime_dir "$LIBEXEC_DIR/scripts"
  for f in "$REPO_AGENTHALO/scripts/"*.swift; do
    [ -f "$f" ] || continue
    install -m 0755 "$f" "$LIBEXEC_DIR/scripts/$(basename "$f")"
  done
  echo "==> installed native workers into $LIBEXEC_DIR/scripts"
fi

# 2) Create the fresh AgentHalo state tree. Removing any previous product is an
# explicit operator step; this installer never imports or aliases old state.
"$SUPERVISOR" stop agenthalo-log-upload >/dev/null 2>&1 || true
"$SUPERVISOR" stop agenthalo >/dev/null 2>&1 || true
mkdir -p "$STATE_DIR/sockets" "$STATE_DIR/data" "$STATE_DIR/screenshots"
chmod 700 "$STATE_DIR" "$STATE_DIR/sockets" 2>/dev/null || true

# 3) config.json was validated before any installed runtime was changed. ------

# The computer-use desktop helper is a separate resident process, not a worker
# script: it owns the display shield as windows it holds, so it has to live in
# the user's GUI session. It travels inside the agent binary and is written out
# here, after config.json exists and before the LaunchAgent is registered — the
# registration validates both paths.
#
# Installing the helper does not enable anything. Computer use still requires
# config.json, and Locked Use additionally requires the separately installed
# authorization plug-in (see docs/computer-use-locked-user.md).
if [ "$PLATFORM" = "Darwin" ]; then
  if DESKTOP_HELPER="$("$RUNTIME_BIN" desktop install 2>/dev/null)"; then
    echo "==> installed desktop helper $DESKTOP_HELPER"
    if [ "${AGENTHALO_SKIP_DESKTOP_LAUNCHAGENT:-0}" = "1" ]; then
      echo "==> NOTE: skipped desktop LaunchAgent registration by explicit request"
    elif [ -x "$REPO_AGENTHALO/mac/launchagent/install.sh" ] && [ "$(id -u)" != "0" ]; then
      DESKTOP_TARGET="gui/$(id -u)/dev.linsheng.agenthalo.desktop"
      if launchctl print "$DESKTOP_TARGET" >/dev/null 2>&1; then
        # Do not let the installer bootout a live helper: bootout does not
        # prove that its shield/relock cleanup completed. The agent startup
        # path performs the atomic prepare_restart handshake before kickstart.
        echo "==> AgentHalo desktop LaunchAgent already loaded; deferring its safe restart to AgentHalo startup"
      else
        if ! bash "$REPO_AGENTHALO/mac/launchagent/install.sh" \
          --config "$CFG" --helper "$DESKTOP_HELPER"; then
          echo "AgentHalo desktop LaunchAgent failed its startup readiness check" >&2
          exit 1
        fi
      fi
    else
      echo "==> NOTE: run mac/launchagent/install.sh --config $CFG as the logged-in user"
      echo "    to start the desktop helper; a LaunchAgent belongs to a user session."
    fi
  else
    echo "==> no embedded desktop helper in this build; computer use will report unavailable"
  fi
fi

# 4) socket dir --------------------------------------------------------------
mkdir -p "$(dirname "$UDS")"

# 5) supervisor drop-in (NEVER touches services.yaml) ------------------------
mkdir -p "$ETC_DIR/services.d"
DROPIN="$ETC_DIR/services.d/agenthalo.yaml"
LOG_SOURCE="$HOME/Library/Logs/private-services/agenthalo.log"
LOG_STATE="$STATE_DIR/data/log-upload-state.json"
infer_log_user() {
  local f base rest
  for f in /opt/private-tunnel/certs/agent-*-"$DEVICE_ID".crt; do
    [ -f "$f" ] || continue
    base="$(basename "$f" .crt)"
    rest="${base#agent-}"
    rest="${rest%-${DEVICE_ID}}"
    [ -n "$rest" ] && printf '%s\n' "$rest" && return 0
  done
  for f in /opt/private-tunnel/cert/*-agent.crt; do
    [ -f "$f" ] || continue
    base="$(basename "$f" .crt)"
    rest="${base%-agent}"
    [ -n "$rest" ] && printf '%s\n' "$rest" && return 0
  done
  return 1
}
if [ -z "$LOG_USER" ]; then
  LOG_USER="$(infer_log_user || true)"
fi
if [ -z "$LOG_CERT_DIR" ]; then
  if [ -d /opt/private-tunnel/certs ]; then
    LOG_CERT_DIR="/opt/private-tunnel/certs"
  elif [ -d /opt/private-tunnel/cert ]; then
    LOG_CERT_DIR="/opt/private-tunnel/cert"
  else
    LOG_CERT_DIR="/opt/private-tunnel/certs"
  fi
fi
{
  echo "# Managed by AgentHalo deploy/install.sh — registers the AI desktop"
  echo "# agent with the private-services supervisor via a drop-in, so the shared"
  echo "# services.yaml is never edited. Re-run install.sh to regenerate."
  echo "services:"
  echo "  agenthalo:"
  echo "    cmd:"
  echo "      - $RUNTIME_BIN"
  echo "      - --config"
  echo "      - $CFG"
  echo "    cwd: $LIBEXEC_DIR"
  if [ -n "$UPDATE_RELAY_URL" ] || [ -n "$UPDATE_CERT_DIR" ]; then
    echo "    env:"
    if [ -n "$UPDATE_RELAY_URL" ]; then
      printf "      AGENTHALO_UPDATE_RELAY_URL: "
      yaml_quote "$UPDATE_RELAY_URL"
      printf "\n"
    fi
    if [ -n "$UPDATE_CERT_DIR" ]; then
      printf "      AGENTHALO_UPDATE_CERT_DIR: "
      yaml_quote "$UPDATE_CERT_DIR"
      printf "\n"
    fi
  fi
  if [ "$LOG_UPLOAD" = "1" ]; then
    echo "  agenthalo-log-upload:"
    echo "    cmd:"
    echo "      - $RUNTIME_BIN"
    echo "      - logs"
    echo "      - upload"
    echo "      - --relay-url"
    echo "      - $LOG_RELAY_URL"
    echo "      - --namespace"
    echo "      - $LOG_NAMESPACE"
    echo "      - --device"
    echo "      - $DEVICE_ID"
    if [ -n "$LOG_USER" ]; then
      echo "      - --user"
      echo "      - $LOG_USER"
    fi
    echo "      - --cert-dir"
    echo "      - $LOG_CERT_DIR"
    echo "      - --state"
    echo "      - $LOG_STATE"
    echo "      - --interval"
    echo "      - $LOG_INTERVAL"
    echo "      - --max-chunk"
    echo "      - $LOG_MAX_CHUNK"
    echo "      - --source"
    echo "      - $LOG_SOURCE"
    echo "    cwd: $LIBEXEC_DIR"
  fi
} > "$DROPIN"
echo "==> wrote drop-in $DROPIN"

# 5b) Claude lifecycle + native interaction observer hooks (idempotent) -----
AGENTHALO_TURNSTATE_DIR="${AGENTHALO_TURNSTATE_DIR:-$HOME/.claude/agenthalo-turnstate}"
AGENTHALO_INTERACTION_DIR="${AGENTHALO_INTERACTION_DIR:-$HOME/.claude/agenthalo-interactions}"
install -d -m 0700 "$AGENTHALO_TURNSTATE_DIR" "$AGENTHALO_INTERACTION_DIR"
if [ "${AGENTHALO_SKIP_HOOK_INSTALL:-0}" != "1" ]; then
  ( cd "$LIBEXEC_DIR" && "$RUNTIME_BIN" hook install-turnstate --binary "$RUNTIME_BIN" \
      --turnstate-dir "$AGENTHALO_TURNSTATE_DIR" --interaction-dir "$AGENTHALO_INTERACTION_DIR" ) \
    && echo "==> installed Claude hooks (turnstate=$AGENTHALO_TURNSTATE_DIR interactions=$AGENTHALO_INTERACTION_DIR)"
fi

# 6) reload supervisor + start agenthalo (never the container agent) ----------
if [ -x "$SUPERVISOR" ]; then
  start_ok=1
  "$SUPERVISOR" reload-config >/dev/null 2>&1 || start_ok=0
  # reload-config auto-starts newly added always-on services. `start` is
  # idempotent for that case and also starts an unchanged stopped service;
  # `restart` here would kill the just-spawned process and introduce a
	# backoff/health-check race during deployment.
	"$SUPERVISOR" start agenthalo >/dev/null 2>&1 || start_ok=0
	if [ "$LOG_UPLOAD" = "1" ]; then
	  "$SUPERVISOR" start agenthalo-log-upload >/dev/null 2>&1 || start_ok=0
	fi
	echo "==> supervisor reloaded + AgentHalo services started"
  if [ "$start_ok" != "1" ]; then
    ready=0
  elif [ "${AGENTHALO_SKIP_HEALTH_CHECK:-0}" != "1" ] && command -v curl >/dev/null 2>&1; then
    ready=0
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if curl --silent --fail --unix-socket "$UDS" http://localhost/healthz >/dev/null; then
        ready=1
        break
      fi
      sleep 1
    done
  else
    ready=1
  fi
  if [ "$ready" != "1" ]; then
	  echo "AgentHalo health check failed: $UDS; removing the failed fresh-install drop-in" >&2
	  rm -f "$DROPIN"
	  "$SUPERVISOR" reload-config >/dev/null 2>&1 || true
	  exit 1
  fi
else
  echo "==> NOTE: supervisor not found at $SUPERVISOR; start $RUNTIME_BIN manually"
fi

mkdir -p "$(dirname "$VERSION_STATE")"
printf '%s\n' "$MODULE_VERSION" >"$VERSION_STATE.tmp"
mv "$VERSION_STATE.tmp" "$VERSION_STATE"

echo "==> done. version=AgentHalo.$MODULE_VERSION UI: https://<user>-relay.<domain>/s/agenthalo/d/$DEVICE_ID/"
exit 0
