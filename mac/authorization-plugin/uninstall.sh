#!/usr/bin/env bash
# Removes the Locked Use Authorization Plug-in and all of its trust state.
#
# Order matters: the mechanism is removed from the authorization right FIRST,
# so the SecurityAgent stops referencing a bundle that is about to disappear.
# A right pointing at a missing mechanism is the one way this feature could
# make a Mac harder to unlock, so it is removed while the bundle is still there.
set -euo pipefail

PLUGIN_DIR="/Library/Security/SecurityAgentPlugins"
BUNDLE_NAME="RemoteAgentLockedUse.bundle"
STATE_DIR="/Library/Application Support/remote-agent/locked-use"
RIGHT="system.login.screensaver"
RULE_NAME="com.psyche08.remote-agent.locked-use"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "Locked Use is macOS-only" >&2
  exit 1
fi
if [ "$(id -u)" != "0" ]; then
  echo "run with sudo: sudo ./uninstall.sh" >&2
  exit 1
fi

echo "==> removing the mechanism from $RIGHT"
TMP="$(mktemp -t ra-lockeduse)"
trap 'rm -f "$TMP" "$TMP.new"' EXIT
BACKUP="$STATE_DIR/$RIGHT.original.plist"
if [ -f "$BACKUP" ]; then
  # Restoring the exact pre-install right is more reliable than editing the
  # current one, which another tool may have changed since.
  if security authorizationdb write "$RIGHT" < "$BACKUP"; then
    echo "==> restored the original $RIGHT from backup"
    rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"
    rm -rf "$STATE_DIR"
    # Also remove the standalone rule definition. Restoring the right from
    # backup drops the *reference* to this rule, but the rule itself lives in
    # the authorization database independently and outlives the right — an
    # orphan naming a now-removed bundle makes a later reinstall behave
    # differently from a first one. This ran only in the fallback path before
    # the exit below, so a normal backup-restore left the orphan behind.
    security authorizationdb remove "$RULE_NAME" >/dev/null 2>&1 && \
      echo "==> removed rule $RULE_NAME" || true
    echo
    echo "==> done. Screen unlock is back to stock macOS behavior."
    exit 0
  fi
  echo "==> backup restore failed; falling back to editing the current right" >&2
fi
if security authorizationdb read "$RIGHT" > "$TMP" 2>/dev/null; then
  # Both shapes have to be handled. The right is normally a rule list holding
  # this plug-in's rule *name*; stripping only a `mechanisms` key would report
  # success and leave the branch in place.
  /usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" <<'PYEOF'
import plistlib, sys
src, dst, rule_name = sys.argv[1], sys.argv[2], sys.argv[3]
with open(src, "rb") as f:
    right = plistlib.load(f)
if "rule" in right:
    right["rule"] = [r for r in right["rule"] if r != rule_name]
if "mechanisms" in right:
    right["mechanisms"] = [
        m for m in right["mechanisms"] if "RemoteAgentLockedUse" not in m
    ]
with open(dst, "wb") as f:
    plistlib.dump(right, f)
PYEOF
  security authorizationdb write "$RIGHT" < "$TMP.new"
  echo "==> unregistered"
else
  echo "==> could not read $RIGHT; leaving it alone"
fi

# The rule definition outlives the right that referenced it, and an orphan rule
# naming a bundle that is gone is exactly the kind of leftover that makes a
# later reinstall behave differently from a first one.
security authorizationdb remove "$RULE_NAME" >/dev/null 2>&1 && \
  echo "==> removed rule $RULE_NAME" || true

echo "==> removing the bundle"
rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"

echo "==> removing trust state (public key + consumed-nonce ledger)"
rm -rf "$STATE_DIR"

echo
echo "==> done. Screen unlock is back to stock macOS behavior."
echo "    Also set computer_use.locked_use.enabled=false in the agent's"
echo "    config.json so it stops trying to arm."
