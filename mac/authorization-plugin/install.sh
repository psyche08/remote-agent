#!/usr/bin/env bash
# Installs the Locked Use Authorization Plug-in and provisions its trust state.
#
# This script changes how your Mac's screen unlocks. Read docs/computer-use-locked-user.md
# before running it. It must be run by an administrator, on the Mac itself, and
# it is reversible with uninstall.sh.
#
# This installer owns only the AgentHalo authorization identity. Before a
# product-identity change, run the uninstaller shipped with the previously
# installed version and verify its rule and bundle are gone; this script does
# not migrate or remove another product's authorization records.
#
# What it does:
#   1. copies the signed bundle to /Library/Security/SecurityAgentPlugins
#   2. creates root-owned state (grant, public key, ledger, nonce receipt)
#   3. installs the agent's PUBLIC key — the private half never leaves the agent
#   4. inserts the mechanism into the system.login.screensaver right
#
# What it deliberately does NOT do:
#   * It never touches the password mechanism or reads a credential. A missing
#     or invalid grant declines this branch; the normal password branch remains.
#   * It does not enable Locked Use. That is a separate opt-in in config.json.
#   * Installation alone never unlocks the Mac. At runtime, loginwindow must
#     evaluate this branch while a fresh valid grant exists; only that one
#     authorization transaction may return Allow without a login password.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_NAME="AgentHaloLockedUse.bundle"
BUILD_DIR="${AGENTHALO_PLUGIN_BUILD_DIR:-$HERE/build}"
BUNDLE="$BUILD_DIR/$BUNDLE_NAME"
PLUGIN_IDENTIFIER="dev.linsheng.agenthalo.locked-use.plugin"
PLUGIN_DIR="/Library/Security/SecurityAgentPlugins"
STATE_DIR="/Library/Application Support/AgentHalo/locked-use"
RIGHT="system.login.screensaver"
MECHANISM="AgentHaloLockedUse:invoke,privileged"
# The mechanism gets its own rule rather than being appended to the right, so
# it can be one branch of the right's k-of-n evaluation. See the registration
# step for why that distinction decides whether it works at all.
RULE_NAME="dev.linsheng.agenthalo.locked-use"
PUBKEY_SOURCE="${1:-}"
DEVICE_ID="${AGENTHALO_DEVICE_ID:-}"
# The account the agent's desktop helper runs as. It has to be able to fill in
# the grant file without owning it — see the grant hand-off below.
AGENT_USER="${AGENTHALO_AGENT_USER:-${SUDO_USER:-}}"
EXPECTED_TEAM_ID="${AGENTHALO_EXPECTED_TEAM_ID:-${AGENTHALO_SIGN_TEAM_ID:-}}"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "Locked Use is macOS-only" >&2
  exit 1
fi
if [ "$(id -u)" != "0" ]; then
  echo "run with sudo: sudo ./install.sh [public-key-file]" >&2
  exit 1
fi
if [ ! -d "$BUNDLE" ]; then
  echo "missing $BUNDLE — run ./build.sh first" >&2
  exit 1
fi
if [ -z "$AGENT_USER" ] || ! id -u "$AGENT_USER" >/dev/null 2>&1; then
  echo "AGENTHALO_AGENT_USER must name the exact account that runs the desktop helper" >&2
  exit 2
fi
if [ -z "$DEVICE_ID" ]; then
  echo "AGENTHALO_DEVICE_ID is required; refusing an authorization rule without device binding" >&2
  exit 2
fi
if [ "${#DEVICE_ID}" -gt 128 ] || [[ ! "$DEVICE_ID" =~ ^[A-Za-z0-9._:-]+$ ]]; then
  echo "AGENTHALO_DEVICE_ID must be 1..128 characters from A-Z a-z 0-9 . _ : -" >&2
  exit 2
fi
if [ -z "$PUBKEY_SOURCE" ] || [ ! -f "$PUBKEY_SOURCE" ]; then
  echo "a provisioned public-key file is required: sudo ./install.sh <public-key-file>" >&2
  exit 2
fi
if ! PUBKEY_BYTES="$(/usr/bin/base64 -D < "$PUBKEY_SOURCE" 2>/dev/null \
      | /usr/bin/wc -c | /usr/bin/tr -d '[:space:]')"; then
  echo "public key is not valid base64" >&2
  exit 2
fi
if [ "$PUBKEY_BYTES" != "65" ]; then
  echo "public key must decode to a 65-byte P-256 X9.63 point (got $PUBKEY_BYTES)" >&2
  exit 2
fi

# Refuse an unsigned bundle. The SecurityAgent loads this into a privileged
# context; an unverifiable bundle there is not something to install quietly.
if ! codesign --verify --strict "$BUNDLE" 2>/dev/null; then
  echo "bundle signature does not verify; refusing to install" >&2
  exit 1
fi

# A syntactically valid signature is not an authorship identity: `codesign -s -`
# produces an ad-hoc bundle that also passes --verify.  This plug-in executes in
# SecurityAgent, so installation requires an explicit Team pin and the exact
# identifier used by the helper's peer audit.
SIGN_INFO="$(codesign -d --verbose=4 "$BUNDLE" 2>&1)"
ACTUAL_IDENTIFIER="$(printf '%s\n' "$SIGN_INFO" | sed -n 's/^Identifier=//p' | head -1)"
ACTUAL_TEAM_ID="$(printf '%s\n' "$SIGN_INFO" | sed -n 's/^TeamIdentifier=//p' | head -1)"
SIGNATURE_KIND="$(printf '%s\n' "$SIGN_INFO" | sed -n 's/^Signature=//p' | head -1)"
if [ "$ACTUAL_IDENTIFIER" != "$PLUGIN_IDENTIFIER" ]; then
  echo "bundle identifier ${ACTUAL_IDENTIFIER:-missing} does not match $PLUGIN_IDENTIFIER" >&2
  exit 1
fi
if [ -z "$EXPECTED_TEAM_ID" ]; then
  echo "AGENTHALO_EXPECTED_TEAM_ID is required and must match the signed agent/helper TeamIdentifier" >&2
  exit 2
fi
if [ -z "$ACTUAL_TEAM_ID" ] || [ "$ACTUAL_TEAM_ID" != "$EXPECTED_TEAM_ID" ]; then
  echo "bundle TeamIdentifier ${ACTUAL_TEAM_ID:-missing} does not match AGENTHALO_EXPECTED_TEAM_ID=$EXPECTED_TEAM_ID" >&2
  exit 1
fi
if [ "$SIGNATURE_KIND" = "adhoc" ]; then
  echo "ad-hoc authorization plug-in bundles are development-only and cannot be installed" >&2
  exit 1
fi

# This edits the authorization right that governs unlocking. A mistake in that
# configuration is the one failure mode that could leave a Mac hard to unlock,
# so the operator has to acknowledge the recovery path before we touch it.
if [ "${AGENTHALO_LOCKED_USE_ACK:-}" != "1" ]; then
  cat >&2 <<'WARN'
This installs a mechanism into the macOS screensaver-unlock authorization right.

The plug-in returns Allow only for a verified one-use grant. Otherwise it denies
this branch and the ordinary password branch remains available. The principal
risk is still the authorization-database edit, so keep a recovery route.

Before running this on a Mac you care about:
  1. Try it on a spare Mac or a VM running the same macOS version.
  2. Keep a second admin account, or Recovery access, available.
  3. Know the reversal: boot to another admin account (or Recovery) and run
     mac/authorization-plugin/uninstall.sh, or restore the right with:
       security authorizationdb write system.login.screensaver < backup.plist
  4. Note that a fresh install does NOT bypass the password on its own. See
     docs/computer-use-locked-user.md before relying on the unlock behavior.

Re-run with AGENTHALO_LOCKED_USE_ACK=1 once you have a recovery path.
WARN
  exit 2
fi

echo "==> installing $BUNDLE_NAME"
mkdir -p "$PLUGIN_DIR"
rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"
cp -R "$BUNDLE" "$PLUGIN_DIR/$BUNDLE_NAME"
chown -R root:wheel "$PLUGIN_DIR/$BUNDLE_NAME"
chmod -R go-w "$PLUGIN_DIR/$BUNDLE_NAME"
codesign --verify --strict "$PLUGIN_DIR/$BUNDLE_NAME"
INSTALLED_SIGN_INFO="$(codesign -d --verbose=4 "$PLUGIN_DIR/$BUNDLE_NAME" 2>&1)"
INSTALLED_IDENTIFIER="$(printf '%s\n' "$INSTALLED_SIGN_INFO" | sed -n 's/^Identifier=//p' | head -1)"
INSTALLED_TEAM_ID="$(printf '%s\n' "$INSTALLED_SIGN_INFO" | sed -n 's/^TeamIdentifier=//p' | head -1)"
if [ "$INSTALLED_IDENTIFIER" != "$PLUGIN_IDENTIFIER" ] || \
   [ "$INSTALLED_TEAM_ID" != "$EXPECTED_TEAM_ID" ]; then
  echo "installed bundle identity changed during copy; refusing authorizationdb migration" >&2
  exit 1
fi

echo "==> preparing $STATE_DIR"
mkdir -p "$STATE_DIR/consumed"
chown -R root:wheel "$STATE_DIR"
# The plug-in reads these as root and rejects anything not root-owned.
chmod 0755 "$STATE_DIR"
chmod 0700 "$STATE_DIR/consumed"

# Three exact-nonce proofs written by the root-context plug-in and read by the
# desktop controller. `receipt.pending` is durable before Allow; `receipt` is
# durable after SetResult(Allow) succeeds; `receipt.complete` is written only
# when that successful mechanism instance is destroyed. The controller cannot
# forge them: the directory is root-owned and no proof is group/other-writable.
for proof in receipt.pending receipt receipt.complete; do
  : > "$STATE_DIR/$proof"
  chown root:wheel "$STATE_DIR/$proof"
  chmod 0644 "$STATE_DIR/$proof"
done

# The grant hand-off crosses a privilege boundary, and this is where it is made
# possible without adding a privileged component.
#
# The plug-in refuses any grant file that is not root-owned, so it can trust
# what it reads. The desktop helper runs as the user: it cannot create a
# root-owned file, cannot write into this directory, and cannot unlink from it.
# Publishing by rename is therefore impossible — the renamed file would be the
# user's, and the plug-in would reject every grant.
#
# So the file is created here, root-owned with a write ACL for the exact helper
# account, and the helper fills it in place. Ownership never changes, so the
# plug-in's check still holds. Withdrawal truncates it to zero, which the plug-in
# already rejects, because unlinking would need write permission on this directory.
#
# Without this the feature installs cleanly, reports armed, and never unlocks.
: > "$STATE_DIR/grant.json"
chown root:wheel "$STATE_DIR/grant.json"
chmod -N "$STATE_DIR/grant.json" 2>/dev/null || true
chmod 0600 "$STATE_DIR/grant.json"
# macOS accounts commonly share the `staff` primary group. A 0620 hand-off
# would let every such account read/write and flock the grant, enabling a
# cross-user denial of service. A named-user ACL grants only this helper account
# the write/truncate capability it needs while ownership remains root:wheel.
chmod +a "user:$AGENT_USER allow write" "$STATE_DIR/grant.json"
echo "==> prepared the grant hand-off ACL for $AGENT_USER"

# Binds grants to this Mac. Without it a grant minted for one device would
# verify on any device provisioned with the same key.
printf '%s\n' "$DEVICE_ID" > "$STATE_DIR/device_id"
chown root:wheel "$STATE_DIR/device_id"
chmod 0600 "$STATE_DIR/device_id"
echo "==> bound grants to device_id=$DEVICE_ID"

install -o root -g wheel -m 0600 "$PUBKEY_SOURCE" "$STATE_DIR/public.key"
echo "==> installed public key"

echo "==> registering the mechanism in $RIGHT"
TMP="$(mktemp -t agenthalo-lockeduse)"
trap 'rm -f "$TMP" "$TMP.new" "$TMP.current" "$TMP.rule" "$TMP.rule.actual"' EXIT
security authorizationdb read "$RIGHT" > "$TMP" 2>/dev/null
# Keep a restorable copy of the original right next to the trust state.
if [ ! -f "$STATE_DIR/$RIGHT.original.plist" ]; then
  cp "$TMP" "$STATE_DIR/$RIGHT.original.plist"
  chown root:wheel "$STATE_DIR/$RIGHT.original.plist"
  chmod 0600 "$STATE_DIR/$RIGHT.original.plist"
  echo "==> backed up the original right to $STATE_DIR/$RIGHT.original.plist"
fi

# Register as a branch of the right, the way macOS actually evaluates it.
#
# system.login.screensaver is a `rule` class right whose rule list is evaluated
# k-of-n with k = 1: the first branch that authorizes wins, and
# `use-login-window-ui` — the password prompt — is the last branch. So the
# mechanism goes into its own evaluate-mechanisms rule, and that rule's name
# goes into the list ahead of the password branch.
#
# Writing a `mechanisms` key onto a rule-class right, which is what this script
# used to do, is silently discarded: the plug-in installs, the script reports
# success, and nothing is ever loaded. If the right is not one of the two shapes
# understood here, this refuses rather than writing something meaningless.
cat > "$TMP.rule" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>class</key><string>evaluate-mechanisms</string>
  <key>comment</key><string>Screen-unlock branch that authorizes only against a fresh, single-use, signed Locked Use grant.</string>
  <key>mechanisms</key><array><string>$MECHANISM</string></array>
  <key>tries</key><integer>1</integer>
  <!-- A successful one-use grant must never become a reusable authorization
       credential in the login session's global cache. -->
  <key>shared</key><false/>
  <key>timeout</key><integer>0</integer>
  <key>version</key><integer>1</integer>
</dict>
</plist>
PLIST
security authorizationdb write "$RULE_NAME" < "$TMP.rule"
security authorizationdb read "$RULE_NAME" > "$TMP.rule.actual" 2>/dev/null
if ! /usr/bin/python3 - "$TMP.rule.actual" "$MECHANISM" <<'PYEOF'
import plistlib, sys
with open(sys.argv[1], "rb") as f:
    rule = plistlib.load(f)
expected_mechanism = sys.argv[2]
# Current macOS omits the explicit integer timeout=0 from this
# evaluate-mechanisms rule when it canonicalizes the write.  The durable
# cross-transaction invariant is therefore shared=false; tries=1 plus the
# plug-in's atomic nonce consumption protects the current transaction.  Accept
# only the observed omission or an exact integer zero, never another value.
timeout = rule.get("timeout")
timeout_ok = timeout is None or (type(timeout) is int and timeout == 0)
ok = (rule.get("class") == "evaluate-mechanisms"
      and rule.get("mechanisms") == [expected_mechanism]
      and rule.get("shared") is False
      and timeout_ok
      and type(rule.get("tries")) is int
      and rule.get("tries") == 1)
raise SystemExit(0 if ok else 1)
PYEOF
then
  echo "authorizationdb did not retain the exact non-shared single-use rule" >&2
  exit 1
fi
echo "==> defined and verified single-use rule $RULE_NAME"

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
    first_product = next((i for i, value in enumerate(rules) if value == rule_name), None)
    retained = [value for value in rules if value != rule_name]
    password_at = retained.index("use-login-window-ui")
    if first_product is None:
        at = password_at
    else:
        # Preserve an existing AgentHalo branch's relative position when safe,
        # forcing the migrated branch ahead of the password fallback.
        before_product = sum(1 for value in rules[:first_product] if value != rule_name)
        at = min(before_product, password_at)
    retained.insert(at, rule_name)
    right["rule"] = retained
elif kind == "evaluate-mechanisms":
    mechanisms = right.get("mechanisms", [])
    if not isinstance(mechanisms, list):
        sys.stderr.write("evaluate-mechanisms right has a malformed mechanisms list\n")
        sys.exit(2)
    retained = [value for value in mechanisms if value != mechanism]
    if not retained:
        sys.stderr.write("right has no non-AgentHalo password fallback mechanism\n")
        sys.exit(2)
    right["mechanisms"] = [mechanism] + retained
else:
    sys.stderr.write(
        "unrecognised authorization right shape: class=%r\n"
        "Refusing to modify it. Registering blindly is how a plug-in ends up\n"
        "installed and inert, or worse, wired into the wrong position.\n" % (kind,))
    sys.exit(2)

with open(dst, "wb") as f:
    plistlib.dump(right, f)
PYEOF
then
  echo "could not register the mechanism; the right was left untouched" >&2
  exit 1
fi
security authorizationdb write "$RIGHT" < "$TMP.new"
echo "==> registered $RULE_NAME in $RIGHT"

# Confirm the live right has exactly one AgentHalo branch and that the ordinary
# password path remains reachable.
if ! security authorizationdb read "$RIGHT" > "$TMP.current" 2>/dev/null; then
  echo "could not read $RIGHT after registration; keeping the installed bundle for recovery" >&2
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
          and rules.count(rule_name) == 1
          and "use-login-window-ui" in rules
          and rules.index(rule_name) < rules.index("use-login-window-ui"))
elif kind == "evaluate-mechanisms":
    mechanisms = right.get("mechanisms", [])
    ok = (isinstance(mechanisms, list)
          and mechanisms.count(mechanism) == 1
          and any(value != mechanism for value in mechanisms))
else:
    ok = False
raise SystemExit(0 if ok else 1)
PYEOF
then
  echo "$RIGHT did not retain the exact AgentHalo branch and password fallback" >&2
  exit 1
fi
echo "==> verified: exactly one AgentHalo branch is present in $RIGHT"

echo
echo "==> done."
echo "    The plug-in authorizes ONLY a verified, single-use grant. Every other"
echo "    unlock — including yours, by hand — falls through to the password"
echo "    prompt, which is the branch after this one."
echo
echo "    VALIDATE BEFORE RELYING ON IT: confirm you can still unlock normally,"
echo "    then confirm a Locked Use turn actually unlocks. If it does not, the"
echo "    right's mechanism arrangement needs adjusting for your macOS version;"
echo "    the feature fails closed until then."
echo
echo "    Locked Use is still OFF. To enable it, set computer_use.enabled and"
echo "    computer_use.locked_use.enabled in the agent's config.json, install the"
echo "    agent's public key above, and restart AgentHalo."
echo
echo "    To reverse everything: sudo ./uninstall.sh"
