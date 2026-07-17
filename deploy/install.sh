#!/usr/bin/env bash
# Idempotent installer for the remote-coding AI desktop agent on one Mac.
#
# It registers the agent with the private-services supervisor via a DROP-IN file
# (services.d/remote-coding.yaml) — it NEVER edits or replaces the shared
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
#   4. the supervisor drop-in                   (services.d/remote-coding.yaml)
#   5. reload-config + restart remote-coding    (never the container agent)
#
# Usage:
#   ./install.sh DEVICE_ID [options]
#     --devices a,b,c        fleet device ids for the unified console (default: DEVICE_ID)
#     --uds PATH             socket path (default: /opt/private-tunnel/state/remote-coding/sockets/backend.sock)
#     --agent-config PATH    retired; ingress is owned by private-edge profiles
#     --etc DIR              supervisor config dir (default: /opt/private-tunnel/etc)
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

DEVICE_ID="${1:?usage: install.sh DEVICE_ID [--devices a,b] [--uds path] [--agent-config path]}"
shift || true

REPO_REMOTE_AGENT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # .../remote-agent
ETC_DIR="/opt/private-tunnel/etc"
BIN_DIR="${RC_BIN_DIR:-/opt/private-tunnel/bin}"
UDS="/opt/private-tunnel/state/remote-coding/sockets/backend.sock"
DEVICES="$DEVICE_ID"
PORT=8765
LOG_UPLOAD=1
LOG_USER="${RC_LOG_UPLOAD_USER:-}"
LOG_CERT_DIR="${RC_LOG_UPLOAD_CERT_DIR:-}"
LOG_RELAY_URL="${RC_LOG_UPLOAD_RELAY_URL:-}"
LOG_NAMESPACE="${RC_LOG_UPLOAD_NAMESPACE:-remocoding}"
LOG_INTERVAL="${RC_LOG_UPLOAD_INTERVAL:-60s}"
LOG_MAX_CHUNK="${RC_LOG_UPLOAD_MAX_CHUNK:-1048576}"
# launchd may run with a minimal PATH; keep versioned Homebrew fallbacks.
PY="$(find_tool python3 /opt/homebrew/bin/python3 /usr/local/bin/python3 /usr/bin/python3)"
GO="$(find_tool go /opt/homebrew/bin/go /opt/homebrew/opt/go/bin/go /opt/homebrew/opt/go@1.25/bin/go /opt/homebrew/opt/go@1.24/bin/go /usr/local/bin/go)"
SUPERVISOR="/opt/private-tunnel/bin/private-services"

while [ $# -gt 0 ]; do
  case "$1" in
    --devices) DEVICES="$2"; shift 2 ;;
    --uds) UDS="$2"; shift 2 ;;
    --agent-config) echo "--agent-config is retired; deploy/update private-edge instead" >&2; exit 2 ;;
    --etc) ETC_DIR="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
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
  echo "log upload requires --log-relay-url URL, RC_LOG_UPLOAD_RELAY_URL, or --no-log-upload" >&2
  exit 2
fi

echo "==> remote-coding install: device=$DEVICE_ID repo=$REPO_REMOTE_AGENT"
if [ ! -x "$GO" ]; then
  echo "go not found; install Go or set PATH before running install.sh" >&2
  exit 127
fi
if [ ! -x "$PY" ]; then
  echo "python3 not found; install Python 3 or set PATH before running install.sh" >&2
  exit 127
fi

# state 目录(对齐 state/<svc>/ 惯例;父目录 0700) ------------------------------
STATE_DIR="/opt/private-tunnel/state/remote-coding"
mkdir -p "$STATE_DIR/sockets" "$STATE_DIR/data" "$STATE_DIR/screenshots"
chmod 700 "$STATE_DIR" "$STATE_DIR/sockets" 2>/dev/null || true
# 一次性迁移仓库内旧 data(仅当 state/data 还空)
if [ -d "$REPO_REMOTE_AGENT/data" ] && [ -z "$(ls -A "$STATE_DIR/data" 2>/dev/null)" ]; then
  for f in sessions.json tasks.json; do
    [ -f "$REPO_REMOTE_AGENT/data/$f" ] && cp -n "$REPO_REMOTE_AGENT/data/$f" "$STATE_DIR/data/$f" || true
  done
  echo "==> migrated existing data/*.json -> $STATE_DIR/data"
fi

# 1) Go backend -------------------------------------------------------------
echo "==> building Go backend"
BUILD_COMMIT="$(git -C "$REPO_REMOTE_AGENT" rev-parse --short HEAD 2>/dev/null || echo dev)"
BUILD_AT="$(TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S+08:00)"
BUILDINFO_PKG="github.com/psyche08/remote-agent/internal/buildinfo"
( cd "$REPO_REMOTE_AGENT" && GOCACHE="${GOCACHE:-/private/tmp/remote-agent-gocache}" "$GO" build -trimpath \
  -ldflags "-X ${BUILDINFO_PKG}.Version=${BUILD_COMMIT} -X ${BUILDINFO_PKG}.Commit=${BUILD_COMMIT} -X ${BUILDINFO_PKG}.BuiltAt=${BUILD_AT}" \
  -o bin/remote-coding ./cmd/remote-coding )
echo "==> built $REPO_REMOTE_AGENT/bin/remote-coding"

# Remove artifacts from the retired Desktop wrapper path. Active Claude
# control is the standalone stream-json child owned by this service.
"$SUPERVISOR" stop remote-coding-claude-wrapper-watch >/dev/null 2>&1 || true
rm -f \
  "$STATE_DIR/data/claude-wrapper-watch.enabled" \
  "$ETC_DIR/services.d/remote-coding-claude-wrapper-watch.yaml" \
  "$BIN_DIR/claude-wrapper" \
  "$BIN_DIR/install-claude-wrapper" \
  "$BIN_DIR/watch-claude-wrapper"

# 2) config.json (create from example if missing; never overwrite) -----------
CFG="$REPO_REMOTE_AGENT/config.json"
if [ -f "$CFG" ]; then
  echo "==> config.json exists — leaving it untouched"
else
  echo "==> writing config.json (device_id=$DEVICE_ID, uds=$UDS)"
  DEVICE_ID="$DEVICE_ID" DEVICES="$DEVICES" UDS="$UDS" PORT="$PORT" \
  "$PY" - "$CFG" "$REPO_REMOTE_AGENT/config.example.json" <<'PYEOF'
import json, os, sys
out, example = sys.argv[1], sys.argv[2]
d = json.load(open(example))
d["device_id"] = os.environ["DEVICE_ID"]
d["devices"] = [x for x in os.environ["DEVICES"].split(",") if x]
d["port"] = int(os.environ["PORT"])
d["uds"] = os.environ["UDS"]
json.dump(d, open(out, "w"), ensure_ascii=False, indent=2)
PYEOF
fi

# 3) socket dir --------------------------------------------------------------
mkdir -p "$(dirname "$UDS")"

# 4) supervisor drop-in (NEVER touches services.yaml) ------------------------
mkdir -p "$BIN_DIR" "$ETC_DIR/services.d"
OLD_WATCHDOG_BIN="$BIN_DIR/remote-coding-watchdog"
OLD_WATCHDOG_DROPIN="$ETC_DIR/services.d/remote-coding-watchdog.yaml"
rm -f "$OLD_WATCHDOG_BIN" "$OLD_WATCHDOG_DROPIN"
echo "==> removed legacy external remote-coding watchdog; internal watchdog runs in the agent"

DROPIN="$ETC_DIR/services.d/remote-coding.yaml"
LOG_SOURCE="$HOME/Library/Logs/private-services/remote-coding.log"
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
  echo "# Managed by remote-agent/deploy/install.sh — registers the AI desktop"
  echo "# agent with the private-services supervisor via a drop-in, so the shared"
  echo "# services.yaml is never edited. Re-run install.sh to regenerate."
  echo "services:"
  echo "  remote-coding:"
  echo "    cmd:"
  echo "      - $REPO_REMOTE_AGENT/bin/remote-coding"
  echo "      - --config"
  echo "      - $REPO_REMOTE_AGENT/config.json"
  echo "    cwd: $REPO_REMOTE_AGENT"
  if [ "$LOG_UPLOAD" = "1" ]; then
    echo "  remote-coding-log-upload:"
    echo "    cmd:"
    echo "      - $REPO_REMOTE_AGENT/bin/remote-coding"
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
    echo "    cwd: $REPO_REMOTE_AGENT"
  fi
} > "$DROPIN"
echo "==> wrote drop-in $DROPIN"

# 4b) turn-state hook —— 让 claude 接管能判断会话 turn 是否结束(幂等) --------
RC_TURNSTATE_DIR="${RC_TURNSTATE_DIR:-$HOME/.claude/remote-coding-turnstate}"
mkdir -p "$RC_TURNSTATE_DIR"
( cd "$REPO_REMOTE_AGENT" && "$REPO_REMOTE_AGENT/bin/remote-coding" hook install-turnstate --binary "$REPO_REMOTE_AGENT/bin/remote-coding" --turnstate-dir "$RC_TURNSTATE_DIR" ) \
  && echo "==> installed turn-state hooks (RC_TURNSTATE_DIR=$RC_TURNSTATE_DIR)"

# 5) reload supervisor + restart remote-coding (never the container agent) ----
if [ -x "$SUPERVISOR" ]; then
  "$SUPERVISOR" reload-config >/dev/null 2>&1 || true
  "$SUPERVISOR" restart remote-coding >/dev/null 2>&1 || "$SUPERVISOR" start remote-coding >/dev/null 2>&1 || true
  if [ "$LOG_UPLOAD" = "1" ]; then
    "$SUPERVISOR" restart remote-coding-log-upload >/dev/null 2>&1 || "$SUPERVISOR" start remote-coding-log-upload >/dev/null 2>&1 || true
  fi
  echo "==> supervisor reloaded + remote-coding (re)started"
else
  echo "==> NOTE: supervisor not found at $SUPERVISOR; start remote-coding manually"
fi

echo "==> done. UI: https://<user>-relay.<domain>/s/remotecoding/d/$DEVICE_ID/"
exit 0
