#!/usr/bin/env bash
# Removes the Locked Use Authorization Plug-in and all of its trust state.
#
# Order matters: the mechanism is removed from the authorization right FIRST,
# so the SecurityAgent stops referencing a bundle that is about to disappear.
# A right pointing at a missing mechanism is the one way this feature could
# make a Mac harder to unlock, so it is removed while the bundle is still there.
set -euo pipefail

PLUGIN_DIR="/Library/Security/SecurityAgentPlugins"
BUNDLE_NAME="AgentHaloLockedUse.bundle"
STATE_DIR="/Library/Application Support/AgentHalo/locked-use"
RIGHT="system.login.screensaver"
RULE_NAME="dev.linsheng.agenthalo.locked-use"
MECHANISM="AgentHaloLockedUse:invoke,privileged"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "Locked Use is macOS-only" >&2
  exit 1
fi
if [ "$(id -u)" != "0" ]; then
  echo "run with sudo: sudo ./uninstall.sh" >&2
  exit 1
fi

echo "==> removing the mechanism from $RIGHT"
TMP="$(mktemp -t agenthalo-lockeduse)"
trap 'rm -f "$TMP" "$TMP.new" "$TMP.current" "$TMP.rule"' EXIT
BACKUP="$STATE_DIR/$RIGHT.original.plist"
if [ -f "$BACKUP" ]; then
  echo "==> keeping the original-right backup as an emergency artifact until unregister succeeds"
fi
if ! security authorizationdb read "$RIGHT" > "$TMP" 2>/dev/null; then
  echo "could not read $RIGHT; refusing to remove a bundle that may still be referenced" >&2
  exit 1
fi

# Edit the current right, not an arbitrarily old snapshot. Other products and
# macOS updates may have added legitimate branches after this plug-in was
# installed; restoring the first-install backup would silently erase them.
if ! /usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" "$MECHANISM" <<'PYEOF'
import plistlib, sys
src, dst, rule_name, mechanism = sys.argv[1:5]
with open(src, "rb") as f:
    right = plistlib.load(f)
kind = right.get("class")
if kind == "rule":
    rules = right.get("rule", [])
    if not isinstance(rules, list) or "use-login-window-ui" not in rules:
        sys.stderr.write("rule-class right has no use-login-window-ui password fallback\n")
        sys.exit(2)
    right["rule"] = [value for value in rules if value != rule_name]
elif kind == "evaluate-mechanisms":
    mechanisms = right.get("mechanisms", [])
    if not isinstance(mechanisms, list):
        sys.stderr.write("evaluate-mechanisms right has a malformed mechanisms list\n")
        sys.exit(2)
    retained = [value for value in mechanisms if value != mechanism]
    if not retained:
        sys.stderr.write("refusing to remove the only authorization mechanism\n")
        sys.exit(2)
    right["mechanisms"] = retained
else:
    sys.stderr.write("unsupported authorization right shape: %r\n" % (kind,))
    sys.exit(2)
with open(dst, "wb") as f:
    plistlib.dump(right, f)
PYEOF
then
  echo "could not construct a safe $RIGHT without the AgentHalo branches" >&2
  exit 1
fi
security authorizationdb write "$RIGHT" < "$TMP.new"

# Removing the privileged bundle is safe only after the live right no longer
# references it. A failed or discarded authorizationdb write must leave the
# bundle and state intact for recovery.
if ! security authorizationdb read "$RIGHT" > "$TMP.current" 2>/dev/null; then
  echo "could not verify $RIGHT after unregister; refusing destructive cleanup" >&2
  exit 1
fi
if ! /usr/bin/python3 - "$TMP.current" "$RULE_NAME" "$MECHANISM" <<'PYEOF'
import plistlib, sys
path, rule_name, mechanism = sys.argv[1:4]
with open(path, "rb") as f:
    right = plistlib.load(f)
kind = right.get("class")
if kind == "rule":
    rules = right.get("rule", [])
    ok = (isinstance(rules, list)
          and rule_name not in rules
          and "use-login-window-ui" in rules)
elif kind == "evaluate-mechanisms":
    mechanisms = right.get("mechanisms", [])
    ok = (isinstance(mechanisms, list)
          and mechanism not in mechanisms
          and len(mechanisms) > 0)
else:
    ok = False
raise SystemExit(0 if ok else 1)
PYEOF
then
  echo "$RIGHT still references AgentHalo, or lost its password fallback; refusing destructive cleanup" >&2
  exit 1
fi
echo "==> unregistered from the current $RIGHT"

# The rule definition outlives the right that referenced it, and an orphan rule
# naming a bundle that is gone is exactly the kind of leftover that makes a
# later reinstall behave differently from a first one.
remove_rule_definition() {
  local name="$1"
  if security authorizationdb read "$name" > "$TMP.rule" 2>/dev/null; then
    security authorizationdb remove "$name" >/dev/null
  fi
  if security authorizationdb read "$name" >/dev/null 2>&1; then
    echo "authorization rule $name still exists; refusing destructive cleanup" >&2
    return 1
  fi
  echo "==> removed rule $name"
}
remove_rule_definition "$RULE_NAME"

echo "==> removing the bundle"
rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"

echo "==> removing trust state (public key + consumed-nonce ledger)"
rm -rf "$STATE_DIR"

echo
echo "==> done. The AgentHalo Locked Use branch has been removed."
echo "    Also set computer_use.locked_use.enabled=false in the agent's"
echo "    config.json so it stops trying to arm."
