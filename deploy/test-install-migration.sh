#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/remote-agent-install-test.XXXXXX")"
trap 'rm -rf "$TMP_ROOT"' EXIT

legacy_state="$TMP_ROOT/state/remote-coding"
new_state="$TMP_ROOT/state/remote-agent"
etc_dir="$TMP_ROOT/etc"
bin_dir="$TMP_ROOT/opt-bin"
libexec_dir="$TMP_ROOT/libexec/remote-agent"
home_dir="$TMP_ROOT/home"
supervisor="$TMP_ROOT/private-services"
supervisor_log="$TMP_ROOT/supervisor.log"
legacy_config="$TMP_ROOT/legacy-config.json"
new_config="$TMP_ROOT/config.json"

mkdir -p "$legacy_state/data" "$legacy_state/sockets" "$etc_dir/services.d" "$home_dir"
printf '{"preserved":true}\n' >"$legacy_state/data/sessions.json"
cat >"$legacy_config" <<EOF
{
  "device_id": "device-test",
  "uds": "$legacy_state/sockets/backend.sock",
  "state_dir": "$legacy_state",
  "providers": {
    "claude": {
      "command": "claude",
      "turnstate_dir": "~/.claude/remote-coding-turnstate"
    }
  }
}
EOF
cat >"$etc_dir/services.d/remote-coding.yaml" <<'EOF'
services:
  remote-coding:
    cmd: [/legacy/remote-coding]
EOF
cat >"$supervisor" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$SUPERVISOR_LOG"
exit 0
EOF
chmod 0755 "$supervisor"

HOME="$home_dir" \
SUPERVISOR_LOG="$supervisor_log" \
RA_SUPERVISOR="$supervisor" \
RA_ETC_DIR="$etc_dir" \
RA_BIN_DIR="$bin_dir" \
RA_LIBEXEC_DIR="$libexec_dir" \
RA_STATE_DIR="$new_state" \
RA_LEGACY_STATE_DIR="$legacy_state" \
RA_CONFIG="$new_config" \
RA_LEGACY_CONFIG="$legacy_config" \
RA_TURNSTATE_DIR="$home_dir/.claude/remote-agent-turnstate" \
RA_SKIP_BUILD=1 \
RA_SKIP_HOOK_INSTALL=1 \
RA_SKIP_HEALTH_CHECK=1 \
  bash "$SCRIPT_DIR/install.sh" device-test --no-log-upload

test -f "$new_state/data/sessions.json"
test -L "$legacy_state"
test "$(readlink "$legacy_state")" = "$new_state"
test ! -e "$etc_dir/services.d/remote-coding.yaml"
test -f "$etc_dir/services.d/remote-coding.yaml.remote-agent-migration"
grep -q '^  remote-agent:$' "$etc_dir/services.d/remote-agent.yaml"
grep -q "$libexec_dir/remote-agent" "$etc_dir/services.d/remote-agent.yaml"
test -x "$libexec_dir/remote-agent"
if grep -q "$REPO_ROOT/bin/remote-agent" "$etc_dir/services.d/remote-agent.yaml"; then
  echo "active drop-in references the Git checkout" >&2
  exit 1
fi
jq -e --arg uds "$new_state/sockets/backend.sock" --arg state "$new_state" '
  .uds == $uds and .state_dir == $state and
  .providers.claude.command == "claude" and
  .providers.claude.turnstate_dir == "~/.claude/remote-agent-turnstate"
' "$new_config" >/dev/null
grep -q '^stop remote-coding$' "$supervisor_log"
grep -q '^start remote-agent$' "$supervisor_log"

# A failed health check must restore the backed-up legacy drop-in/service.
fake_bin="$TMP_ROOT/fake-bin"
mkdir -p "$fake_bin"
cat >"$fake_bin/curl" <<'EOF'
#!/bin/sh
exit 1
EOF
chmod 0755 "$fake_bin/curl"
set +e
HOME="$home_dir" \
PATH="$fake_bin:/usr/bin:/bin:/usr/sbin:/sbin" \
SUPERVISOR_LOG="$supervisor_log" \
RA_SUPERVISOR="$supervisor" \
RA_ETC_DIR="$etc_dir" \
RA_BIN_DIR="$bin_dir" \
RA_LIBEXEC_DIR="$libexec_dir" \
RA_STATE_DIR="$new_state" \
RA_LEGACY_STATE_DIR="$legacy_state" \
RA_CONFIG="$new_config" \
RA_LEGACY_CONFIG="$legacy_config" \
RA_TURNSTATE_DIR="$home_dir/.claude/remote-agent-turnstate" \
RA_SKIP_BUILD=1 \
RA_SKIP_HOOK_INSTALL=1 \
  bash "$SCRIPT_DIR/install.sh" device-test --no-log-upload
rollback_rc=$?
set -e
test "$rollback_rc" -ne 0
test ! -e "$etc_dir/services.d/remote-agent.yaml"
test -f "$etc_dir/services.d/remote-coding.yaml"
grep -q '^restart remote-coding$' "$supervisor_log"

echo "remote-agent installer migration test passed"
