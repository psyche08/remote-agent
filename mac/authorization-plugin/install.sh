#!/usr/bin/env bash
# Installs the Locked Use Authorization Plug-in and provisions its trust state.
#
# This script changes how your Mac's screen unlocks. Read docs/computer-use-locked-user.md
# before running it. It must be run by an administrator, on the Mac itself, and
# it is reversible with uninstall.sh.
#
# What it does:
#   1. copies the signed bundle to /Library/Security/SecurityAgentPlugins
#   2. creates the root-owned locked-use directory (grant, public key, ledger)
#   3. installs the agent's PUBLIC key — the private half never leaves the agent
#   4. inserts the mechanism into the system.login.screensaver right
#
# What it deliberately does NOT do:
#   * It never touches the password mechanism or reads a credential. The plug-in
#     only ever declines to object, so unlocking by hand is unchanged.
#   * It does not enable Locked Use. That is a separate opt-in in config.json.
#   * It does not, on its own, make a valid grant bypass the password. See the
#     validation note in docs/computer-use-locked-user.md.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_NAME="RemoteAgentLockedUse.bundle"
BUILD_DIR="${RA_PLUGIN_BUILD_DIR:-$HERE/build}"
BUNDLE="$BUILD_DIR/$BUNDLE_NAME"
PLUGIN_DIR="/Library/Security/SecurityAgentPlugins"
STATE_DIR="/Library/Application Support/remote-agent/locked-use"
RIGHT="system.login.screensaver"
MECHANISM="RemoteAgentLockedUse:invoke,privileged"
# The mechanism gets its own rule rather than being appended to the right, so
# it can be one branch of the right's k-of-n evaluation. See the registration
# step for why that distinction decides whether it works at all.
RULE_NAME="com.psyche08.remote-agent.locked-use"
PUBKEY_SOURCE="${1:-}"
DEVICE_ID="${RA_DEVICE_ID:-}"
# The account the agent's desktop helper runs as. It has to be able to fill in
# the grant file without owning it — see the grant hand-off below.
AGENT_USER="${RA_AGENT_USER:-${SUDO_USER:-}}"

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

# Refuse an unsigned bundle. The SecurityAgent loads this into a privileged
# context; an unverifiable bundle there is not something to install quietly.
if ! codesign --verify --strict "$BUNDLE" 2>/dev/null; then
  echo "bundle signature does not verify; refusing to install" >&2
  exit 1
fi

# This edits the authorization right that governs unlocking. A mistake in that
# configuration is the one failure mode that could leave a Mac hard to unlock,
# so the operator has to acknowledge the recovery path before we touch it.
if [ "${RA_LOCKED_USE_ACK:-}" != "1" ]; then
  cat >&2 <<'WARN'
This installs a mechanism into the macOS screensaver-unlock authorization right.

The plug-in itself never returns Deny or Undefined, so it cannot refuse your
unlock. The residual risk is the authorization-database edit, not the code.

Before running this on a Mac you care about:
  1. Try it on a spare Mac or a VM running the same macOS version.
  2. Keep a second admin account, or Recovery access, available.
  3. Know the reversal: boot to another admin account (or Recovery) and run
     mac/authorization-plugin/uninstall.sh, or restore the right with:
       security authorizationdb write system.login.screensaver < backup.plist
  4. Note that a fresh install does NOT bypass the password on its own. See
     docs/computer-use-locked-user.md before relying on the unlock behavior.

Re-run with RA_LOCKED_USE_ACK=1 once you have a recovery path.
WARN
  exit 2
fi

echo "==> installing $BUNDLE_NAME"
mkdir -p "$PLUGIN_DIR"
rm -rf "${PLUGIN_DIR:?}/$BUNDLE_NAME"
cp -R "$BUNDLE" "$PLUGIN_DIR/$BUNDLE_NAME"
chown -R root:wheel "$PLUGIN_DIR/$BUNDLE_NAME"
chmod -R go-w "$PLUGIN_DIR/$BUNDLE_NAME"

echo "==> preparing $STATE_DIR"
mkdir -p "$STATE_DIR/consumed"
chown -R root:wheel "$STATE_DIR"
# The plug-in reads these as root and rejects anything not root-owned.
chmod 0755 "$STATE_DIR"
chmod 0700 "$STATE_DIR/consumed"

# The grant hand-off crosses a privilege boundary, and this is where it is made
# possible without adding a privileged component.
#
# The plug-in refuses any grant file that is not root-owned, so it can trust
# what it reads. The desktop helper runs as the user: it cannot create a
# root-owned file, cannot write into this directory, and cannot unlink from it.
# Publishing by rename is therefore impossible — the renamed file would be the
# user's, and the plug-in would reject every grant.
#
# So the file is created here, root-owned and writable by the agent's group, and
# the helper fills it in place. Ownership never changes, so the plug-in's check
# still holds. Withdrawal truncates it to zero, which the plug-in already
# rejects, because unlinking would need write permission on this directory.
#
# Without this the feature installs cleanly, reports armed, and never unlocks.
if [ -n "$AGENT_USER" ]; then
  AGENT_GROUP="$(id -gn "$AGENT_USER" 2>/dev/null || echo staff)"
  : > "$STATE_DIR/grant.json"
  chown "root:$AGENT_GROUP" "$STATE_DIR/grant.json"
  chmod 0620 "$STATE_DIR/grant.json"
  echo "==> prepared the grant hand-off for $AGENT_USER (group $AGENT_GROUP)"
else
  echo "==> NOTE: no agent user known; the helper will not be able to publish grants." >&2
  echo "    Re-run with RA_AGENT_USER=<the account the agent runs as>." >&2
fi

if [ -n "$DEVICE_ID" ]; then
  # Binds grants to this Mac. Without it a grant minted for one device would
  # verify on any device provisioned with the same key.
  printf '%s\n' "$DEVICE_ID" > "$STATE_DIR/device_id"
  chown root:wheel "$STATE_DIR/device_id"
  chmod 0600 "$STATE_DIR/device_id"
  echo "==> bound grants to device_id=$DEVICE_ID"
else
  echo "==> NOTE: no RA_DEVICE_ID given; grants will not be device-bound."
  echo "    Set RA_DEVICE_ID=<device_id from config.json> and re-run to bind them."
fi

if [ -n "$PUBKEY_SOURCE" ]; then
  if [ ! -f "$PUBKEY_SOURCE" ]; then
    echo "public key file not found: $PUBKEY_SOURCE" >&2
    exit 1
  fi
  install -o root -g wheel -m 0600 "$PUBKEY_SOURCE" "$STATE_DIR/public.key"
  echo "==> installed public key"
else
  echo "==> NOTE: no public key provided."
  echo "    Locked Use stays inert until you install one:"
  echo "      curl --unix-socket <agent.sock> http://localhost/computer_use  # read locked_use.public_key"
  echo "      sudo install -o root -g wheel -m 0600 <key-file> '$STATE_DIR/public.key'"
fi

echo "==> registering the mechanism in $RIGHT"
TMP="$(mktemp -t ra-lockeduse)"
trap 'rm -f "$TMP" "$TMP.new"' EXIT
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
  <key>shared</key><true/>
  <key>version</key><integer>1</integer>
</dict>
</plist>
PLIST
security authorizationdb write "$RULE_NAME" < "$TMP.rule"
echo "==> defined rule $RULE_NAME"

set +e
/usr/bin/python3 - "$TMP" "$TMP.new" "$RULE_NAME" "$MECHANISM" <<'PYEOF'
import plistlib, sys
src, dst, rule_name, mechanism = sys.argv[1:5]
with open(src, "rb") as f:
    right = plistlib.load(f)

kind = right.get("class")
if kind == "rule":
    rules = right.get("rule", [])
    if rule_name in rules:
        sys.exit(0)
    # Ahead of the password prompt, but behind any other agent's branch, so
    # installing this does not preempt something already in the chain.
    try:
        at = rules.index("use-login-window-ui")
    except ValueError:
        at = len(rules)
    rules.insert(at, rule_name)
    right["rule"] = rules
elif kind == "evaluate-mechanisms":
    mechanisms = right.get("mechanisms", [])
    if mechanism in mechanisms:
        sys.exit(0)
    mechanisms.insert(0, mechanism)
    right["mechanisms"] = mechanisms
else:
    sys.stderr.write(
        "unrecognised authorization right shape: class=%r\n"
        "Refusing to modify it. Registering blindly is how a plug-in ends up\n"
        "installed and inert, or worse, wired into the wrong position.\n" % (kind,))
    sys.exit(2)

with open(dst, "wb") as f:
    plistlib.dump(right, f)
PYEOF
REGISTER_STATUS=$?
set -e
if [ "$REGISTER_STATUS" != "0" ]; then
  echo "could not register the mechanism; the right was left untouched" >&2
  exit 1
fi
if [ -f "$TMP.new" ]; then
  security authorizationdb write "$RIGHT" < "$TMP.new"
  echo "==> registered $RULE_NAME in $RIGHT"
else
  echo "==> already registered"
fi

# Confirm rather than assume: the previous version reported success after a
# write the system discarded.
if security authorizationdb read "$RIGHT" 2>/dev/null | grep -q "$RULE_NAME"; then
  echo "==> verified: $RULE_NAME is present in $RIGHT"
else
  echo "the right does not contain $RULE_NAME after writing it" >&2
  exit 1
fi

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
echo "    agent's public key above, and restart remote-agent."
echo
echo "    To reverse everything: sudo ./uninstall.sh"
