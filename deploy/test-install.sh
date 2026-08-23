#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agenthalo-install-test.XXXXXX")"
trap 'rm -rf "$TMP_ROOT"' EXIT

state_dir="$TMP_ROOT/state/agenthalo"
etc_dir="$TMP_ROOT/etc"
libexec_dir="$TMP_ROOT/libexec/agenthalo"
home_dir="$TMP_ROOT/home"
supervisor="$TMP_ROOT/private-services"
supervisor_log="$TMP_ROOT/supervisor.log"
codesign="$TMP_ROOT/codesign"
codesign_log="$TMP_ROOT/codesign.log"
config="$TMP_ROOT/config.json"
update_cert_dir="$TMP_ROOT/update certs"
update_relay_url="https://relay.example.test:8443"

mkdir -p "$etc_dir/services.d" "$home_dir" "$update_cert_dir"
cat >"$supervisor" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$SUPERVISOR_LOG"
exit 0
EOF
chmod 0755 "$supervisor"
cat >"$codesign" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$CODESIGN_LOG"
if [ "$1" = "--verify" ]; then
  [ "${MOCK_CODESIGN_VERIFY:-ok}" = "ok" ]
  exit $?
fi
if [ "$1" = "-d" ]; then
  echo "Identifier=${MOCK_CODESIGN_IDENTIFIER:-dev.linsheng.agenthalo}" >&2
  echo "TeamIdentifier=${MOCK_CODESIGN_TEAM:-TESTTEAM}" >&2
  echo 'CodeDirectory v=20500 flags=0x10000(runtime)' >&2
  exit 0
fi
exit 0
EOF
chmod 0755 "$codesign"

run_install() {
  HOME="$home_dir" \
  SUPERVISOR_LOG="$supervisor_log" \
  AGENTHALO_SUPERVISOR="$supervisor" \
  AGENTHALO_ETC_DIR="$etc_dir" \
  AGENTHALO_LIBEXEC_DIR="$libexec_dir" \
  AGENTHALO_STATE_DIR="$state_dir" \
  AGENTHALO_CONFIG="$config" \
  AGENTHALO_TURNSTATE_DIR="$home_dir/.claude/agenthalo-turnstate" \
  AGENTHALO_PLATFORM=Darwin \
  AGENTHALO_CODESIGN="$codesign" \
  AGENTHALO_EXPECTED_TEAM_ID=TESTTEAM \
  CODESIGN_LOG="$codesign_log" \
  AGENTHALO_SKIP_BUILD=1 \
  AGENTHALO_SKIP_DESKTOP_LAUNCHAGENT=1 \
  AGENTHALO_SKIP_HOOK_INSTALL=1 \
  AGENTHALO_SKIP_HEALTH_CHECK=1 \
    bash "$SCRIPT_DIR/install.sh" device-test --no-log-upload "$@"
}

AGENTHALO_UPDATE_RELAY_URL="$update_relay_url" \
AGENTHALO_UPDATE_CERT_DIR="$update_cert_dir" \
  run_install

# A signed prebuilt release is the supported source-free deployment artifact.
# The target Mac must not need a Go toolchain when AGENTHALO_SKIP_BUILD=1.
PATH="/usr/bin:/bin:/usr/sbin:/sbin" \
AGENTHALO_UPDATE_RELAY_URL="$update_relay_url" \
AGENTHALO_UPDATE_CERT_DIR="$update_cert_dir" \
  run_install

dropin="$etc_dir/services.d/agenthalo.yaml"
test -d "$state_dir/data"
test -d "$state_dir/sockets"
test -x "$libexec_dir/agenthalo"
test -f "$dropin"
test "$(stat -f '%Lp' "$config")" = "600"
grep -q '^  agenthalo:$' "$dropin"
grep -q "$libexec_dir/agenthalo" "$dropin"
grep -Fq "      AGENTHALO_UPDATE_RELAY_URL: '$update_relay_url'" "$dropin"
grep -Fq "      AGENTHALO_UPDATE_CERT_DIR: '$update_cert_dir'" "$dropin"
jq -e --arg uds "$state_dir/sockets/backend.sock" --arg state "$state_dir" '
  .uds == $uds and .state_dir == $state and
  .providers.claude.turnstate_dir == "~/.claude/agenthalo-turnstate" and
  .providers.claude.primary_route == "desktop_computer_use" and
  .providers.claude.fallback_route == "stream_json_cli" and
  .providers.claude.desktop_bundle_id == "com.anthropic.claudefordesktop" and
  .providers.claude.desktop_team_id == "Q6L2SF6YDW" and
  .providers.codex.shared_daemon_autostart == true and
  .providers.catpaw.type == "catpaw" and
  .providers.catpaw.app_path == "~/Applications/CatPaw.app" and
  .providers.catpaw.history_db_path == "~/.sankuai/MCopilot/sqliteDB/globalCache.sqlite" and
  .computer_use.enabled == true and
  .computer_use.locked_use.enabled == true and
  .computer_use.debug_http_actions == false and
  .computer_use.helper_socket == "~/Library/Application Support/AgentHalo/desktop.sock"
' "$config" >/dev/null
grep -q '^start agenthalo$' "$supervisor_log"
grep -Fq -- "--verify --strict --verbose=2 $REPO_ROOT/bin/agenthalo" "$codesign_log"

# A prebuilt macOS binary is installable only when its exact identifier, team,
# signature validation and hardened-runtime flag all match.
runtime_sha="$(shasum -a 256 "$libexec_dir/agenthalo" | awk '{print $1}')"
for bad_signature in wrong_identifier wrong_team unsigned; do
  set +e
  case "$bad_signature" in
    wrong_identifier) MOCK_CODESIGN_IDENTIFIER=com.example.impostor run_install ;;
    wrong_team) MOCK_CODESIGN_TEAM=WRONGTEAM run_install ;;
    unsigned) MOCK_CODESIGN_VERIFY=fail run_install ;;
  esac
  signature_rc=$?
  set -e
  test "$signature_rc" -ne 0
  test "$(shasum -a 256 "$libexec_dir/agenthalo" | awk '{print $1}')" = "$runtime_sha"
done

# Source checkout paths are not installed runtime identity. The validator must
# allow a legitimate project root whose repository directory keeps the old Go
# module name.
jq '.project_roots = ["/Users/test/Developer/remote-agent"]' "$config" >"$config.tmp"
mv "$config.tmp" "$config"
run_install
jq -e '.project_roots == ["/Users/test/Developer/remote-agent"]' "$config" >/dev/null

# An explicitly selected source must exist and must be a fresh AgentHalo
# config. Old product paths and the removed plaintext signing-key entry are
# rejected before the LaunchAgent or supervisor service can be installed.
old_config="$TMP_ROOT/old-config.json"
old_target="$TMP_ROOT/rejected-config.json"
old_etc_dir="$TMP_ROOT/rejected-etc"
cat >"$old_config" <<'EOF'
{
  "state_dir": "/opt/private-tunnel/state/remote-agent",
  "computer_use": {"locked_use": {"signing_key_path": "/tmp/old.key"}}
}
EOF
set +e
HOME="$home_dir" \
SUPERVISOR_LOG="$supervisor_log" \
AGENTHALO_SUPERVISOR="$supervisor" \
AGENTHALO_ETC_DIR="$old_etc_dir" \
AGENTHALO_LIBEXEC_DIR="$TMP_ROOT/rejected-libexec" \
AGENTHALO_STATE_DIR="$TMP_ROOT/rejected-state" \
AGENTHALO_CONFIG="$old_target" \
AGENTHALO_CONFIG_SOURCE="$old_config" \
AGENTHALO_TURNSTATE_DIR="$home_dir/.claude/agenthalo-turnstate" \
AGENTHALO_PLATFORM=Darwin \
AGENTHALO_CODESIGN="$codesign" \
AGENTHALO_EXPECTED_TEAM_ID=TESTTEAM \
CODESIGN_LOG="$codesign_log" \
AGENTHALO_SKIP_BUILD=1 \
AGENTHALO_SKIP_DESKTOP_LAUNCHAGENT=1 \
AGENTHALO_SKIP_HOOK_INSTALL=1 \
AGENTHALO_SKIP_HEALTH_CHECK=1 \
  bash "$SCRIPT_DIR/install.sh" device-test --no-log-upload
old_config_rc=$?
set -e
test "$old_config_rc" -ne 0
test ! -e "$old_target"
test ! -e "$old_etc_dir/services.d/agenthalo.yaml"

# Explicit installer options override the environment and are persisted.
cli_update_relay_url="https://cli-relay.example.test:8443"
cli_update_cert_dir="$TMP_ROOT/cli update certs"
mkdir -p "$cli_update_cert_dir"
AGENTHALO_UPDATE_RELAY_URL="https://ignored.example.test" \
AGENTHALO_UPDATE_CERT_DIR="$TMP_ROOT/ignored-certs" \
  run_install --update-relay-url "$cli_update_relay_url" --update-cert-dir "$cli_update_cert_dir"
grep -Fq "      AGENTHALO_UPDATE_RELAY_URL: '$cli_update_relay_url'" "$dropin"
grep -Fq "      AGENTHALO_UPDATE_CERT_DIR: '$cli_update_cert_dir'" "$dropin"
if grep -Fq "ignored.example.test" "$dropin"; then
  echo "CLI update settings did not override environment values" >&2
  exit 1
fi

# With no update setting, a regenerated fresh-install drop-in stays opt-in.
run_install
if grep -q 'AGENTHALO_UPDATE_' "$dropin"; then
  echo "installer enabled auto-update without an explicit setting" >&2
  exit 1
fi

# A failed health check removes only the failed AgentHalo drop-in; there is no
# old product fallback in a fresh-only install.
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
AGENTHALO_SUPERVISOR="$supervisor" \
AGENTHALO_ETC_DIR="$etc_dir" \
AGENTHALO_LIBEXEC_DIR="$libexec_dir" \
AGENTHALO_STATE_DIR="$state_dir" \
AGENTHALO_CONFIG="$config" \
AGENTHALO_TURNSTATE_DIR="$home_dir/.claude/agenthalo-turnstate" \
AGENTHALO_PLATFORM=Darwin \
AGENTHALO_CODESIGN="$codesign" \
AGENTHALO_EXPECTED_TEAM_ID=TESTTEAM \
CODESIGN_LOG="$codesign_log" \
AGENTHALO_SKIP_BUILD=1 \
AGENTHALO_SKIP_DESKTOP_LAUNCHAGENT=1 \
AGENTHALO_SKIP_HOOK_INSTALL=1 \
  bash "$SCRIPT_DIR/install.sh" device-test --no-log-upload
failure_rc=$?
set -e
test "$failure_rc" -ne 0
test ! -e "$dropin"

echo "AgentHalo fresh-install test passed"
