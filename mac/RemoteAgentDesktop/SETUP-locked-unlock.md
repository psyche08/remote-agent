# Locked Use end-to-end setup (on-device, user-run)

Everything below runs on the target Mac and needs things an agent must not do
on your behalf: your login-keychain password, your real login password, and an
actual unlock of your machine. The code is complete and unit-tested; these are
the remaining human-owned steps. Run them yourself.

Before starting, understand what you are enabling: a resident helper will hold
your macOS login password in the data-protection keychain and submit it to
unlock the Mac when an authorized Locked Use turn arrives. That is a real,
deliberate capability. `--clear-unlock-credential` (step 5) removes it at any
time.

Repo path used below:
`/Users/sheng/Developer/Projects/remote-agent/.claude/worktrees/continue-task-362974`

## 1. Unlock the login keychain (so Developer ID signing can reach its key)

Locked-screen signing fails with `errSecInternalComponent` because the login
keychain is locked with the screen. Unlocking the keychain does not unlock the
screen.

```bash
security unlock-keychain ~/Library/Keychains/login.keychain-db
```

It prompts for your login password. Verify:

```bash
security show-keychain-info ~/Library/Keychains/login.keychain-db
# should print timeout/lock settings, not a passphrase error
```

## 2. Build and install the Developer-ID-signed helper

The keychain-access-group entitlement only resolves under Developer ID signing
(an ad-hoc build carrying it is SIGKILLed at launch). Use your identity:

```bash
cd .../mac/RemoteAgentDesktop
RA_SIGN_IDENTITY="Developer ID Application: Sheng Lin (89LGY6BD53)" \
  bash build.sh --out ~/Library/Application\ Support/remote-agent/bin/remote-agent-desktop
```

Verify the signature and entitlement:

```bash
codesign --verify --strict ~/Library/Application\ Support/remote-agent/bin/remote-agent-desktop && echo "signature ok"
codesign -d --entitlements - ~/Library/Application\ Support/remote-agent/bin/remote-agent-desktop | grep -A2 keychain-access
# expect: 89LGY6BD53.com.psyche08.remote-agent.locked-use
```

## 3. Provision your login password (from stdin — never argv, never the socket)

```bash
~/Library/Application\ Support/remote-agent/bin/remote-agent-desktop --set-unlock-credential
# type your macOS login password at the prompt; it is not echoed and not stored in shell history
```

Verify it stored (presence only; the value is never returned):

```bash
~/Library/Application\ Support/remote-agent/bin/remote-agent-desktop --self-check >/dev/null 2>&1
# no error means the entitled binary can reach the data-protection keychain
```

## 4. Restart the LaunchAgent and run the round trip

```bash
launchctl kickstart -k gui/$(id -u)/com.psyche08.remote-agent-desktop
SOCK="$HOME/Library/Application Support/remote-agent/desktop.sock"
```

Lock the screen (Ctrl-Cmd-Q or the menu). Then, from another session or SSH:

```bash
printf '{"op":"lock_state"}\n' | nc -U "$SOCK"           # expect locked:true
printf '{"op":"window_open","turn_id":"verify-1"}\n' | nc -U "$SOCK"
printf '{"op":"lock_state"}\n' | nc -U "$SOCK"           # expect locked:false if the unlock succeeded
```

Ground-truth check (independent of the helper):

```bash
ioreg -n Root -d1 -a | python3 -c 'import sys,plistlib; d=plistlib.loads(sys.stdin.buffer.read()); [print("locked=",u.get("CGSSessionScreenIsLocked")) for u in (d.get("IOConsoleUsers") or [])]'
```

Watch the authorization chain during the attempt (must be `/usr/bin/log`, not
the zsh builtin):

```bash
/usr/bin/log show --last 2m --style compact --info --debug --predicate 'process == "authd" OR process == "loginwindow"' \
  | grep -iE "running mechanism|Succeeded authorizing|credential_submitted|_authSuccess"
```

A completed unlock shows the three-mechanism chain
(`RemoteAgentLockedUse:invoke` gate → `builtin:reset-password` →
`builtin:authenticate`) and `Unlock succeeded`. The helper's audit ring records
`credential_submitted`. If the credential is wrong or absent, the screen simply
stays locked — the safe direction.

## 5. Teardown

```bash
~/Library/Application\ Support/remote-agent/bin/remote-agent-desktop --clear-unlock-credential
```

To remove the whole feature: `sudo mac/authorization-plugin/uninstall.sh`, then
`mac/launchagent/install.sh --uninstall`.

## What is verified vs. not

- Verified without a real password: the mechanism (from this machine's logs),
  the credential-custody logic (86 tests), and three deployment constraints
  found by running an ad-hoc build. See docs/locked-unlock-investigation.md.
- Not verified: the on-device round trip above. It requires your password and a
  real unlock, so it is intentionally left to you rather than run by an agent.
