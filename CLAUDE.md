# remote-agent/CLAUDE.md

Harness for the macOS local agent driving AI coding apps. The production path
is the Go `bin/remote-agent` service over UDS; it translates web/mobile
requests into local Claude/Codex session operations.

Repository, executable and supervisor service identity are all `remote-agent`.
The public relay route `/s/remotecoding/` remains a compatibility URL only.

## Always On

1. This file contains the repository-local maintenance rules.
2. In the reference deployment, the macOS-native agent runs under a supervisor; ingress
   runs in the device's Apple Container private-edge profile. Deploy remote-agent
   by publishing a release to the relay (`deploy/publish-release.sh`) — devices
   configured with `RC_UPDATE_RELAY_URL` check the relay manifest every 5 minutes. private-edge updates
   remain git-based and independent of the `remote-agent` binary release.
3. Treat user-facing Claude/Codex as provider-managed agent sessions, not a tmux
   UI model. The registry exposes canonical `claude` (managed standalone
   stream-json CLI; Desktop is discovery metadata plus an explicit process
   handoff) and `codex` (a per-session app-server or Desktop-IPC delivery route).
   A Desktop-owned Codex session must not fall back to another app-server owner.
4. Scope status, approval, questions, running state, and manual takeover commands
   by `provider_id` + `session_id`; avoid provider-global state leaks.
5. Relay HTTP timeout is 30s. Long waits must be bounded or moved out of the
   request path.
6. Do not log account, cookie, token, recovery-code, or file contents.

## Route Context

| Task | Read |
|---|---|
| Current provider registry, identities, delivery routes and invariants | [docs/provider-architecture.md](docs/provider-architecture.md) |
| General project README | [README.md](README.md) |
| Go API/config/state/provider code | `internal/` |
| Web console | [static/index.html](static/index.html) |

## Deploy Notes

Every release (backend binary + embedded device UI) goes through ONE script, run from a
checkout at the commit to ship, on a network that can SSH to the relay host:

```bash
cd remote-agent && bash deploy/publish-release.sh relay.example.com
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

It cross-builds `remote-agent-darwin-arm64` with the full device console
embedded and uploads it plus `assets/release/manifest.json` (commit + build
datetime in UTC+8; manifest uploaded last) to the relay release directory.
Each configured device agent compares the manifest against its `/healthz`
version every 5 minutes; on mismatch it downloads `assets/release/update.sh` + the binary
(sha256-verified, agent mTLS cert), swaps the binary atomically, and restarts
via the supervisor. `deploy/install.sh` remains first-install bootstrap only.

The relay service root is a deliberately stable device-host shell. It selects
an available device and frames that device's embedded console without leaving
the root PWA URL. Normal releases must not overwrite it; publish it only when
the host, manifest, icons, or root service worker itself changes:

```bash
RC_PUBLISH_SHELL=1 bash deploy/publish-release.sh relay.example.com
```
