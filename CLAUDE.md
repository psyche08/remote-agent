# AgentHalo / CLAUDE.md

AgentHalo is the macOS local agent driving AI coding apps. The production path
is the Go `bin/agenthalo` service over UDS; it translates web/mobile
requests into local Claude/Codex session operations.

The product/signing identity is AgentHalo / `dev.linsheng.agenthalo`.
The installed command, supervisor service and public relay namespace are all
`agenthalo`. The existing Go module path remains unchanged only until the Git
repository is moved; it is not an installed compatibility identity.

## Always On

1. This file contains the repository-local maintenance rules.
2. In the reference deployment, the macOS-native agent runs under a supervisor; ingress
   runs in the device's Apple Container private-edge profile. Deploy AgentHalo
   by publishing a release to the relay (`deploy/publish-release.sh`) — devices
   configured with `AGENTHALO_UPDATE_RELAY_URL` check the relay manifest every 5 minutes. private-edge updates
   remain git-based and independent of the `agenthalo` binary release.
3. Treat user-facing Claude/Codex as provider-managed agent sessions, not a tmux
   UI model. The registry exposes canonical `claude` (session-sticky
   `desktop_computer_use` primary; managed `stream_json_cli` is only a
   brand-new-session, pre-UI-mutation fallback) and `codex` (a per-session
   app-server or Desktop-IPC delivery route). A Desktop-owned Claude/Codex
   session must not fall back to a second owner.
4. Scope status, approval, questions, running state, and manual takeover commands
   by `provider_id` + `session_id`; avoid provider-global state leaks.
5. Relay HTTP timeout is 30s. Long waits must be bounded or moved out of the
   request path.
6. Do not log account, cookie, token, recovery-code, or file contents.
7. Fresh-install config explicitly enables Computer Use and Locked Use; a
   missing/disabled block still fails closed, and config remains the ceiling —
   no API call may enable it or install the plug-in. Locked Use participates in
   the macOS unlock flow through an Apple Authorization Plug-in: it never
   touches the password, is transparent without a valid grant, and mints only
   seconds-long single-use signed grants. Every safeguard fails toward
   relocking; there is no log-and-continue branch. See the doc above before
   touching `internal/computeruse/` or `mac/authorization-plugin/`.
8. Claude Desktop mutations run only inside short in-process Computer Use /
   Locked Use transactions. Before every mutation require a fresh AX read and
   exact bundle id, Team id, logical/native session and request id. Always close
   and relock synchronously; observer-hook/transcript polling must never open a
   window. Only a remote human may answer `AskUserQuestion` or select the
   smallest one-time Claude tool allow/deny. Never enter macOS credentials,
   approve TCC/Touch ID, answer SSO/MFA, or select Always/session-wide access.
9. CLI fallback is decided before any UI mutation and only for a brand-new
   session. After any possible Desktop mutation, an existing Desktop owner,
   local input, identity mismatch or another safety refusal, return a terminal
   error (`delivery_unknown` when delivery is ambiguous); never resend through
   CLI.
10. Do not infer locked-use completion from unit tests, signing, install state,
    health, or an unlocked smoke test. The target Mac must separately pass
    locked Claude prompt, question, one-time allow/deny, terminal relock, and
    no-duplicate-fallback E2E; `m4pro` remains pending until that evidence is
    collected.

## Route Context

| Task | Read |
|---|---|
| Current provider registry, identities, delivery routes and invariants | [docs/provider-architecture.md](docs/provider-architecture.md) |
| Computer use, Locked Use, the unlock grant contract and its threat model | [docs/computer-use-locked-user.md](docs/computer-use-locked-user.md) |
| General project README | [README.md](README.md) |
| Go API/config/state/provider code | `internal/` |
| Web console | [static/index.html](static/index.html) |

## Deploy Notes

Every release (backend binary + embedded device UI) goes through ONE script, run from a
checkout at the commit to ship, on a network that can SSH to the relay host:

```bash
cd /path/to/agenthalo && bash deploy/publish-release.sh relay.example.com
```

Publishing requires `NOTARY_TEAM_ID`, `NOTARY_APPLE_ID`, and
`NOTARY_PASSWORD` in the login-shell environment. The script selects a
Developer ID Application identity whose certificate team exactly matches
`NOTARY_TEAM_ID`, signs the Darwin executable with hardened runtime and a
timestamp, and requires an Accepted notarization result for its ZIP payload
before upload. Devices verify the same embedded team id plus manifest sha256;
they do not ad-hoc re-sign downloaded binaries. (`spctl --type execute` is not
a valid acceptance check for a bare CLI Mach-O: it reports “not an app” even
when notarytool accepted the signed payload.)

It builds and signs the macOS desktop helper first and embeds it, so a release
stays one artifact with one sha256 and one signing team — the helper holds the
Accessibility and Screen Recording grants, and TCC binds those to a code
signature, so it must carry the same Developer ID. It then cross-builds
`agenthalo-darwin-arm64` with that helper and the full device console
embedded and uploads it plus `assets/release/manifest.json` (independent
integer module version + source commit + build datetime in UTC+8; manifest
uploaded last) to the relay release directory. `VERSION` defines the module's
baseline; publishing defaults to the relay's current `module_version + 1`, and
an install deployment similarly advances its target-local deployment version
only after health succeeds. A failed deployment does not consume a version.
Each configured device agent compares the manifest against its `/healthz`
version every 5 minutes; on mismatch it downloads `assets/release/update.sh` + the binary
(sha256-verified, agent mTLS cert), swaps the binary atomically, and restarts
via the supervisor. `deploy/install.sh` remains first-install bootstrap only.

The relay service root is a deliberately stable device-host shell. It selects
an available device and frames that device's embedded console without leaving
the root PWA URL. Normal releases must not overwrite it; publish it only when
the host, manifest, icons, or root service worker itself changes:

```bash
AGENTHALO_PUBLISH_SHELL=1 bash deploy/publish-release.sh relay.example.com
```
