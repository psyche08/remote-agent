# AgentHalo

**AgentHalo** is the product name. Its primary macOS signing/bundle identifier is
`dev.linsheng.agenthalo`.

The Git remote and Go module still use `github.com/psyche08/remote-agent`
until the repository itself is moved. That source import path is not an
installed product identity. A fresh installation otherwise uses AgentHalo
names only: `agenthalo` for the command/service/relay namespace,
`/opt/private-tunnel/state/agenthalo` for state, and `AGENTHALO_*` for its
operator environment.

The macOS identities are derived from the base identifier:

- agent: `dev.linsheng.agenthalo`
- desktop helper and fresh LaunchAgent: `dev.linsheng.agenthalo.desktop`
- Authorization Plug-in: `dev.linsheng.agenthalo.locked-use.plugin`
- authorization child rule: `dev.linsheng.agenthalo.locked-use`

This is a clean replacement, not an in-place identity migration. Before
installing AgentHalo, use the uninstaller from the previously installed
version and verify that its LaunchAgent, Authorization rule, plug-in bundle and
state are gone. AgentHalo neither imports nor accepts the previous product's
identity or state. See
[`SETUP-locked-unlock.md`](mac/RemoteAgentDesktop/SETUP-locked-unlock.md).

A small **macOS local agent** that lets you drive AI coding/chat agents on a Mac
from a phone/browser — **without** RDP, VNC, Parsec or any remote-desktop/video
protocol.

Each Mac logs into one isolated AI account and runs one agent. The agent feeds
tasks to the AI app and reads the output back, through a **provider/adapter**
layer:

| Family | Providers | How it drives the agent | How it reads output |
|--------|-----------|-------------------------|---------------------|
| **Claude** | `claude` | drives the exact Claude Desktop session through short in-process Computer Use / Locked Use transactions; a managed `stream-json` CLI is a pre-mutation fallback for a brand-new session only | side-effect-free observer hooks + merged Desktop/CLI transcript metadata; CLI NDJSON only on the sticky fallback route |
| **Codex Desktop/app-server** | `codex` | maps each logical session to a Codex thread and binds mutable control to the official shared app-server daemon; Desktop owner/follower IPC is an explicit compatibility route | merged app-server/local discovery, app-server notifications, and rollout preview tailing |
| **Generic terminal fallback** | configured `"type": "pty"` providers | starts one fixed executable + args in one PTY per logical session; no shell expansion | bounded, terminal-control-sanitized memory + WebSocket deltas |

> **Account isolation is solved at the device layer.** One Mac ≈ one Claude
> account or one Codex account. The agent never switches or logs into accounts.

Claude exposes one provider and one identity namespace. Desktop session metadata
and CLI transcript metadata are merged by Claude transcript id. A fresh logical
session defaults to the session-sticky `desktop_computer_use` route: prompt input,
`AskUserQuestion` answers, and one-time tool allow/deny decisions are applied to
the exact Claude Desktop session through bounded in-process Computer Use / Locked
Use transactions. A managed CLI `stream-json` child is only a preselected
`stream_json_cli` fallback when capability preflight for a brand-new session fails
before any UI mutation. The legacy `claude_cli` and `claude_desktop` ids resolve
to `claude`; they are not separate owners or retry routes.

All provider paths are intended for real coding work: Claude keeps Desktop as the
primary owner and observes durable hook/transcript state without unlocking it;
its documented stdin/stdout `stream-json` protocol remains the constrained
fallback. Codex uses app-server because it matches Codex Desktop / VS Code's
thread-turn-item model and exposes native streaming, steer, and interrupt
operations.

```
agenthalo/
├── go.mod
├── Makefile
├── README.md
├── cmd/agenthalo/              # Go service entrypoint
├── internal/                    # Go API/config/state/provider implementation
├── config.example.json          # copy to config.json and edit
├── data/                         # local runtime state (ignored by Git)
├── static/
│   ├── shell.html               # stable relay device picker
│   └── index.html               # full console embedded into the agent binary
├── mac/
│   ├── preflight.sh             # on-Mac checks for computer use / Locked Use
│   ├── RemoteAgentDesktop/      # resident desktop helper: shield, safeguards, grants
│   └── authorization-plugin/    # Locked Use Apple Authorization Plug-in (built on the Mac)
├── scripts/
│   └── ocr_vision.swift         # local Apple Vision OCR worker
├── deploy/
│   └── private-tunnel.example.yaml   # how to expose the agent via ../private-tunnel
└── screenshots/
    └── .gitkeep
```

## Architecture: provider / adapter

The production agent is the Go binary `bin/agenthalo`. Its registry exposes
canonical `claude` and `codex` providers. Claude binds each logical session to a
sticky Desktop-Computer-Use or CLI-fallback route and merges Desktop/CLI
discovery. Codex binds each logical session to either an app-server or
Desktop-IPC delivery route.

The complete current model — provider contract, logical/native identity,
discovery/runtime merge, delivery ownership, streaming and approvals — is in
[`docs/provider-architecture.md`](docs/provider-architecture.md).

The Go `Provider` interface owns status/model metadata, native discovery and
preview, logical session lifecycle, prompt/output/state, approvals, interrupt
and model selection. Small optional interfaces add attachments, transcript
assets, runtime sessions, native resume/fork, precise request-scoped approvals,
Desktop delivery binding and Codex message rewind without bloating the base
contract.

### Generic PTY providers

Use `"type": "pty"` only when an interactive agent has no structured protocol.
`command` and `args` are executed directly, never through a shell, and each
logical session owns one child process and one PTY:

```json
"terminal-agent": {
  "type": "pty",
  "app_name": "Terminal Agent",
  "command": "agent-cli",
  "args": ["--interactive"],
  "cwd": "~/Developer",
  "prompt_suffix": "\r",
  "interrupt_sequence": "\u0003",
  "idle_timeout_ms": 1500,
  "ready_pattern": "(?m)^> $",
  "allow_raw_keys": false,
  "max_output_bytes": 262144,
  "max_sessions": 32
}
```

This adapter intentionally advertises fewer capabilities: no native resume,
structured approvals/questions, attachments, steer, or model controls. Output
and preview history are memory-bounded and are not a durable transcript.
Completion is best-effort, based on `ready_pattern` or output quiet time.
Prefer native Claude/Codex providers whenever their structured protocol is
available.

### claude provider — Desktop Computer Use first

* **The delivery route belongs to the logical session.** A fresh session starts
  with `desktop_computer_use`. Before the first desktop mutation, AgentHalo
  verifies local Computer Use / Locked Use capability and the configured Claude
  Desktop identity. Only a brand-new session whose capability preflight fails
  at that point may be bound once to `stream_json_cli`; that choice remains
  sticky for the session.
* **Desktop input is a short transaction, not a permanently unlocked UI.** Each
  prompt, `AskUserQuestion` answer, or Claude tool permission decision opens one
  bounded in-process Computer Use transaction, targets the exact configured
  bundle id + Team id + logical/native Claude session + request id, performs the
  action, and synchronously closes/relocks before returning. Claude continues
  running while the Mac is locked.
* **Observe before every mutation.** Each set-value, press, click, or key action
  consumes a fresh Accessibility observation. A later action must read the
  current tree again; cached paths, labels, screenshots, and coordinates cannot
  authorize a second mutation.
* **No cross-route replay.** Once opening/activating Desktop or any later UI
  mutation may have happened, a timeout or ambiguous result is
  `delivery_unknown` and is never resent through CLI. An existing Desktop owner,
  local physical input, owner/session/request mismatch, unavailable shielding,
  or any other security refusal also fails closed without CLI fallback.
* **Prompt delivery is restart-safe.** Claude `/send_prompt` requests carry a
  stable `operation_id`. The PWA persists and reads it back before sending, and
  the service records an immutable attempt before the first Desktop or CLI side
  effect. Retrying that operation after a timeout or restart never sends it a
  second time; an unresolved attempt is reported as `delivery_unknown`.
* **Questions and approval remain human decisions.** Observer hooks/transcripts
  expose a pending request without opening the screen. Only `/question_answer`
  or `/approval`, carrying the exact session and request id, starts the UI
  transaction. `allow` means the smallest one-time/“Allow Once” Claude tool
  permission; if Desktop offers only “Always” or session-wide authorization,
  AgentHalo refuses it. It never approves on the model's behalf.
* **“Approval” is not operating-system authentication.** This route never
  enters or stores a macOS password, approves TCC, completes Touch ID, changes
  account/login state, or answers SSO/MFA. Those remain manual-only. `deny` is
  allowed only for the matched Claude tool request.
* **Side-effect-free read path.** `~/.claude/projects`, Claude Desktop's
  `claude-code-sessions` metadata, and the configured observer hook directories
  supply discovery, pending-request, output, and completion state. Polling
  `/status`, `/output`, native sessions, or WebSocket state never unlocks the
  machine. Desktop and CLI rows remain merged by `cliSessionId`.
* **CLI fallback preserves the structured path.** A session selected for
  `stream_json_cli` uses `--session-id`/`--resume`, SDK `user` NDJSON frames,
  `--output-format stream-json`, request-scoped control responses, and
  structured interrupt exactly as before. It is a fallback owner, not a retry
  after Desktop delivery.
* **Per-turn usage**: the transcript preview appends a local annotation after
  every completed turn with input/output/cache-create/cache-read tokens,
  wall-clock duration, and a standard API-price estimate in USD. Repeated
  Claude transcript records for the same streamed API message are deduplicated.

### codex provider — app-server

The registered `codex` provider is the Go `provider.Codex`.

* **Shared daemon owns mutable native control by default**. A logical web
  session maps to one native Codex thread id. AgentHalo resumes that exact
  thread and cwd on the official same-user daemon before starting a turn.
  Desktop owner/follower IPC is used only when
  `extra.native_delivery_route=desktop_ipc` is explicitly configured; in that
  compatibility mode attach also requests a complete pending-request snapshot.
* **Delivery route belongs to the logical session**. New AgentHalo-created
  threads are managed app-server sessions. Sending from a native Codex preview
  persists the explicit `codex_control_route=shared_daemon`, resumes the exact
  thread id/cwd on the official managed daemon, and starts the turn on that
  same connection. `delivery_route=desktop_ipc` is retained temporarily as a
  fail-closed rollback contract for older binaries. Desktop IPC remains
  available only through the explicit `extra.native_delivery_route=desktop_ipc`
  compatibility override; a request is never retried across owner routes.
* **Native reads**: `/native_sessions` merges app-server `thread/list` with the
  local Codex index/rollouts. `/session_preview` reads rollout JSONL first and
  only uses a bounded `thread/resume` fallback when the rollout is not yet on
  disk.
  Session discovery asks app-server for the 200 most recent interactive threads
  from its state DB, then incrementally indexes rollout metadata by
  path/size/mtime. Subagent rows remain internally addressable but do not
  consume the visible-session budget.
* **Convergent list refresh**: `/native_sessions` keeps a last-good snapshot.
  `refresh=1` starts a single background refresh and immediately returns that
  snapshot with `generation`, `refreshing`, `refreshed_at`, and
  `refresh_error`. The PWA renders native rows before live-session enrichment,
  follows `refreshing` with bounded retries, ignores superseded
  device/provider requests, and refreshes every five seconds while visible.
* **Live output**: headless turns publish app-server `item/agentMessage/delta`
  and `thread/status/changed` notifications to both the logical session id and
  native thread id. The PWA live-tails Desktop-owned turns by polling
  `/session_preview`; the Desktop follower bridge supplies owner/running/settings
  and pending-request changes.
* **Approvals bridge over Desktop IPC**. Approval requests for a Desktop-owned
  turn are sent by app-server to the turn's *owner* client, not to
  AgentHalo's shared-daemon connection. A persistent follower connection
  mirrors each owner's `thread-stream-state-changed` broadcasts (snapshot +
  immer patches); the broadcast conversation state carries the raw pending
  server requests (`requests[]`, including the JSON-RPC request id) plus the
  thread's real `approvalPolicy` / `approvalsReviewer` / sandbox / model.
  `/status` exposes them as `approval_request` (with a stable `request_id`)
  and `session_settings`; `POST /approval {request_id, decision}` answers via
  `thread-follower-command-approval-decision` /
  `thread-follower-file-approval-decision` /
  `thread-follower-permissions-request-approval-response` routed to the owner.
  First response wins across Desktop and web; late responses report `stale`.
  `approvalsReviewer=auto_review` turns are guardian-reviewed server-side and
  never surface fake web approvals. Approvals are tracked per thread +
  request id: one thread going idle no longer clears another thread's queue.
* **Codex transport discovery**: when no transport is explicitly configured,
  AgentHalo prefers the official managed standalone install at
  `~/.codex/packages/standalone/current/codex`; otherwise it falls back to a
  usable ChatGPT.app/Codex.app/PATH/common-prefix binary and its stdio
  app-server. Codex and ChatGPT own the shared `CODEX_HOME` authentication
  state, so AgentHalo does not require a second provider login. Install the
  managed standalone with OpenAI's
  `https://chatgpt.com/codex/install.sh`, then run
  `codex app-server daemon start`. Set
  Set `extra.app_server_transport=shared_daemon` to require that layout, and
  `extra.shared_daemon_autostart=true` only on devices where AgentHalo is
  explicitly allowed to start an already-installed daemon after reboot. This
  flag never installs Codex, bootstraps its updater, or enables OpenAI cloud
  remote control. An explicit transport never silently falls back.
* **Shared app-server lifecycle**: the provider assigns every UDS WebSocket
  connection a generation. EOF, malformed JSON, or socket loss immediately
  fails pending RPCs and retires only that generation's routes. An old exit
  callback cannot clear a newer connection, and the daemon itself outlives a
  AgentHalo reconnect.
* **Authoritative completion**: approval/question response writes remain
  non-terminal until the matching typed `serverRequest/resolved` arrives.
  Likewise, `turn/interrupt` reports `cancellation_requested`; the session
  remains running until Codex publishes a terminal event.
* **Per-turn usage**: completed `task_started` / `task_complete` boundaries use
  Codex's native duration and the delta of cumulative token counters, so a
  session total is never repeated as an individual turn's usage.
* **Markdown diagrams**: fenced `mermaid` blocks use a pinned embedded Mermaid
  runtime, strict/no-HTML-label rendering, SVG scrubbing, and a scriptless
  sandbox iframe. Unsafe or invalid diagrams keep their source visible.
  The device UI CSP intentionally blocks arbitrary remote Markdown images;
  transcript images are served through the session-scoped `/session_asset`
  route instead.

### Computer use & Locked Use

Fresh AgentHalo installs explicitly enable both features in the device's own
`config.json`. An omitted `computer_use` block still fails closed, and no API
call can install the plug-in or raise the local configuration ceiling.

* **Computer use is a model tool, not a self-asserted turn id.** New Codex
  threads receive `computer_use.get_app_state/press/set_value/click/type_text/
  press_key/scroll` dynamic tools. Calls are bound to the current app-server
  generation, active thread and authoritative turn; every mutation consumes one
  fresh screenshot + Accessibility-tree observation, so the next mutation must
  inspect again. Provider completion,
  interruption, error or transport loss revokes the lease.
* **Claude uses a separate trusted provider transaction.** It does not invent a
  model turn or reuse the HTTP debug surface. The API binds the canonical
  provider and persisted logical/native session, issues a short-lived internal
  operation lease, and permits only the exact Claude Desktop bundle + Team +
  session + pending request. Prompt input and a human's answer/one-time tool
  decision close and relock synchronously; background observation never opens a
  Locked Use window.
* **The desktop boundary is signed twice.** A resident Swift LaunchAgent owns
  Screen Recording, Accessibility, the display shield and input event tap. Its
  `0600` UDS also checks the peer audit token and accepts only a Go agent with
  the same Developer ID Team and exact signing identifier. Enabled Locked Use
  disables naked HTTP open/action/AX by default; production calls use the
  in-process model broker.
* **Locked Use is an Apple Authorization Plug-in branch.** A valid signed grant
  makes the AgentHalo branch return Allow; missing, invalid, expired or
  replayed grants return Deny and evaluation continues to the normal
  `use-login-window-ui` password branch. The implementation never stores,
  reads or submits a macOS login password.
* **Grants are seconds-long, device-bound, console-user-bound and single-use.**
  Grant v2 signs the active console account's canonical UID and username; the
  privileged plug-in requires both to match the username in that exact
  authorization transaction. The helper keeps its ECDSA P-256 private key in
  the current user's file-based login Keychain. Only the explicit provisioning
  command may create it; the Keychain's default creator ACL binds the item to
  the installed helper's code-signing designated requirement. Normal runtime
  startup only reads the existing item with authentication UI disabled. A
  missing or inaccessible item, including a manually or policy-locked login
  Keychain during helper startup, fails closed without deleting or rotating the
  key. The plug-in receives only the public half. The plug-in consumes
  each nonce with `O_CREAT|O_EXCL` and publishes root-owned `pending`, `final`
  and `complete` proofs: `pending` precedes Allow, `final` follows a successful
  `SetResult(Allow)`, and only `MechanismDestroy` for that successful Allow may
  publish the terminal `complete` proof. The exact lock-screen field must remain
  the same element while the screen stays locked until `complete`; only then may
  its lifecycle completion plus an unlocked state open the model window.
  A coincidental human, Apple Watch or alternate authorization unlock before
  that boundary fails closed instead of being claimed by the model turn.
* **The physical screen stays black while the model sees the UI.** Shielding-
  level, `sharingType=.readOnly` windows cover every display; the signed helper
  explicitly excludes its own shield from the model capture while ordinary
  screen captures retain the black cover;
  frames are returned as bounded in-memory PNG bytes, never replaceable temp
  paths. Model click/scroll coordinates use the composite PNG's top-left origin;
  Swift maps them into Core Graphics global coordinates, including negative-
  origin and vertically arranged displays, and refuses points in display gaps.
  A session event tap drops unmarked physical keyboard/pointer input but
  permits helper-marked synthetic events. The same physical event sets a sticky
  latch and triggers relock on the fixed ~40 ms monitor.
* **Cleanup is a state machine, not best effort.** Opening/open/closing ownership
  is atomic; close waits for in-flight authorization and desktop operations.
  Grant withdrawal, relock and lock-state readback precede shield release.
  Uncertain late authorization enters quarantine, keeps the shield alive and
  retries until the locked boundary is proved. If nonce proof exists but the
  ordered loginwindow UI lifecycle cannot be proved, status reports
  `requires_manual_recovery=true`; that ambiguity is not repaired by a later
  unlocked sample, and the shield remains until controlled reboot/recovery.

Apple's public Authorization Plug-in API does not promise that
`MechanismDestroy` occurs after loginwindow has applied its visible unlock side
effect. The target-Mac release gate must therefore verify the observed
`complete -> unlock` ordering and exercise a concurrent alternate unlock; unit
tests cannot turn that operating-system timing assumption into an API guarantee.
Before `complete`, an alternate unlock is detected and quarantined by the
implementation. After `complete`, however, the public APIs expose neither a
transaction ID for the visible lock transition nor a causal completion callback:
an Apple Watch or another authorization path can produce the same
`field disappeared + unlocked` observations. Until target-Mac testing proves
that the original transaction cannot subsequently apply, deployments must keep
alternate unlock unavailable during unattended use or add an independent
guardian/client-side completion primitive. This boundary is not claimed as a
production guarantee by the current code.

The Swift worker and the authorization plug-in only build and run on macOS, so
CI cannot check them. Run `bash mac/preflight.sh` on the target Mac first — it
is read-only by default and also catches drift in the constants the Go code and
the plug-in must agree on.

This is deliberately **not** a general-purpose remote unlock: it authorizes one
bounded Codex tool or Claude provider transaction, not other applications or
local processes. Claude's Desktop-first route has the same close/relock and
fresh-observation requirements, but the target-Mac locked prompt/question/
one-time-approval E2E is still a release gate and is not claimed complete here.
There is also no independent root deadman yet, so unit tests cannot certify the
real locked-device boundary. The current signature pin identifies trusted code,
not the one managed process instance, so hostile same-UID/TCC environments
require the root broker described in the threat model. Setup, the full contract
and limits are in
[`docs/computer-use-locked-user.md`](docs/computer-use-locked-user.md).

### API-price estimates

The device refreshes standard token prices once per day from the official
Anthropic pricing table and the current OpenAI model pages discovered from the
official model comparison page. The last successful response is cached in
`data/api-pricing.json`; refresh failures retain that cache, then fall back to
an embedded last-known catalog. Unknown/private model aliases display `—`
instead of using a guessed price. These values are API-equivalent estimates;
Claude/Codex subscription usage may not create an API bill.

## API

The agent is intended to sit behind private-tunnel mTLS and a local UDS
filesystem boundary; it does not require an app-layer bearer token in the Go
path.

| Method | Path | Purpose |
|--------|------|---------|
| GET  | `/status` | device, active provider/session, state, last prompt/shot/clip, last error, (approval_request when waiting) |
| GET  | `/providers` | providers + status + boolean capabilities + typed actions |
| POST | `/provider/select` | switch active provider |
| POST | `/send_prompt` | `{provider_id?, session_id, prompt, attachments?}` → drive provider, task → running |
| POST | `/upload` | multipart `provider_id`, `session_id`, `file` → opaque session-scoped attachment (25 MB max) |
| GET  | `/session_asset` | read an image already referenced by the selected provider/session transcript |
| GET  | `/pricing` | pricing refresh time, official/fallback model counts, source URLs, and last refresh error |
| POST | `/screenshot` | capture screen → path/url |
| GET  | `/last_screenshot` | the latest PNG |
| GET  | `/clipboard` | `pbpaste` |
| GET  | `/output` | best-effort latest output (CLI: stream buffer/transcript; GUI: native) |
| POST | `/ocr` | Apple Vision OCR of the latest screenshot |
| POST | `/copy_reply` | Legacy: Copy-button worker (no current provider implements it; returns not_supported) |
| POST | `/recover` | relaunch/activate or re-establish provider session |
| GET  | `/sessions` · POST `/sessions` | list / create logical sessions |
| GET  | `/tasks` | task history |
| GET  | `/pending_approvals` | provider/session-scoped approval and question inbox across every live session |
| POST | `/approval` | `{provider_id, session_id, request_id, decision}` for a Claude or Codex request |
| POST | `/question_answer` | `{provider_id, session_id, request_id, answers}` for provider user-input questions |
| GET  | `/computer_use` | computer-use capability, Locked Use state, and a secret-free audit ring |
| POST | `/computer_use/locked_use` | `{active}` runtime switch, bounded by on-device config |
| POST | `/computer_use/window` | debug open / always-safe close, scoped by provider + session + authoritative turn; Locked Use open is model-only by default |
| POST | `/computer_use/action` | ordinary-unlocked compatibility action; Locked Use is model-only by default |
| POST | `/computer_use/ax` | ordinary-unlocked AX compatibility route; Locked Use is model-only by default |

Typed actions give clients a closed operation id plus `endpoint`, `scope`,
`risk`, and `supported`; legacy boolean capabilities remain for older clients.
Codex subagent threads remain directly addressable by exact id for preview and
control, but are filtered from normal stored/native/live session lists and the
unscoped task list.

## Setup

### 1. Install

```bash
cd /path/to/agenthalo
make go-build
cp config.example.json config.json       # edit device_id, cwd, providers
```

`make go-build` is a local development build; it is not an installable macOS
release identity. A fresh macOS install must use either the signed/notarized
artifact from `deploy/publish-release.sh`, or run `deploy/install.sh` with
`AGENTHALO_EXPECTED_TEAM_ID` and a non-ad-hoc
`AGENTHALO_SIGN_IDENTITY`. The installer verifies the exact identifier
`dev.linsheng.agenthalo`, Team ID and hardened-runtime flag before replacing
the installed binary; a prebuilt binary passed through
`AGENTHALO_SKIP_BUILD=1` is subject to the same checks.

For a manual local release build that does not contact the relay or deploy
anything, pass an explicit integer module version to the repository-root
script:

```bash
./build.sh 10
```

It uses `NOTARY_TEAM_ID` plus either the existing
`NOTARY_APPLE_ID`/`NOTARY_PASSWORD` environment or a
`NOTARY_KEYCHAIN_PROFILE`. The script builds and Developer ID signs the main
binary, desktop helper, and Authorization Plug-in; submits all three in one
Apple notarization payload; and retains the Accepted result and full notary
log. Every retained artifact and build cache is kept under the Git-ignored
`build/` directory. This script never publishes, installs the Plug-in, changes
authorizationdb, or deploys a device, and it never falls back to ad-hoc
signing.

```bash
AGENTHALO_EXPECTED_TEAM_ID=ABCDE12345 \
AGENTHALO_SIGN_IDENTITY="Developer ID Application: AgentHalo (ABCDE12345)" \
  bash deploy/install.sh <device-id> --no-log-upload
```

Claude Desktop, the signed AgentHalo desktop helper, and its local permissions
are the primary `claude` route. A runnable standalone `claude` CLI is optional
but required if a brand-new session is to use the configured pre-mutation
fallback. Mutable Codex control requires OpenAI's managed standalone install and
its local app-server daemon:

```bash
which claude
test -x ~/.codex/packages/standalone/current/codex
~/.codex/packages/standalone/current/codex app-server daemon start
```

### 2. macOS permissions

| Permission | Needed by | Where |
|------------|-----------|-------|
| **Screen Recording** | `screencapture` and the signed desktop helper (Codex observations and Claude Desktop transactions) | System Settings → Privacy & Security → Screen Recording |
| **Accessibility** | synthetic keyboard/pointer/AX actions for computer use and Claude Desktop input | System Settings → Privacy & Security → Accessibility |

The Claude CLI fallback and Codex app-server path need **none** of these for core
protocol operation. Claude's primary Desktop route does: Screen Recording is
needed for fresh observations and Accessibility for input. Neither permission is
granted through Claude's `/approval` action; TCC remains a manual administrator/
user setup step.

Locked Use additionally requires installing the Apple Authorization Plug-in —
see [`docs/computer-use-locked-user.md`](docs/computer-use-locked-user.md). It is
a separate, reversible, admin-only step that changes how the Mac unlocks, and it
stays inert until you also opt in via `config.json`.

### 3. Run

```bash
./bin/agenthalo --config config.json
# -> unix socket from config.json, or http://127.0.0.1:8765 when no uds is set
```

Open `http://127.0.0.1:8765`, pick a provider, create a session, and send a
prompt.

### 4. Reach it from iPhone — via private-tunnel (no public ports)

The agent binds loopback only. Expose it through a private-tunnel-compatible
reverse tunnel (mTLS). Two config edits —
see [deploy/private-tunnel.example.yaml](deploy/private-tunnel.example.yaml):

1. **Relay**: add an `agenthalo` service with `port: 8765` and
   `default_device: <this-mac>` (no `static_dir` — the agent serves its own UI).
   `default_device` makes the relay strip the `/s/agenthalo/` prefix and
   forward clean paths to the agent, so the UI's absolute paths work.
2. **private-edge profile**: keep port `8765` mapped through the dedicated
   vmnet UDS gateway. Do not install or restart a host tunnel agent.

Then on the phone (mTLS client cert installed):
`https://<user>-relay.<domain>/s/agenthalo/`.

The service root serves a relay-owned device host. It keeps the root PWA URL,
chooses the last device from browser storage (or the most recently connected
available device), and loads `/s/agenthalo/d/<device>/` inside a same-origin
frame. The embedded console keeps every tab bound to its own device, so session
switches route status, output, input, approvals, and WebSocket traffic to that
session's agent. Normal UI/backend releases therefore update devices through
the release manifest without rewriting the relay PWA shell.

The security boundary is the local UDS/filesystem permission plus
private-tunnel **mTLS**. Never expose port 8765 directly.

Auto-update and log upload are opt-in in the public repository. Set
`AGENTHALO_UPDATE_RELAY_URL` while running `deploy/install.sh`, or pass
`--update-relay-url`, to persist manifest polling in the `agenthalo`
supervisor drop-in. Set `AGENTHALO_UPDATE_CERT_DIR` or pass `--update-cert-dir` when
the updater's mTLS certificates are outside its default discovery paths.
Pass `--log-relay-url` (or set `AGENTHALO_LOG_UPLOAD_RELAY_URL`) when installing with
log upload, or use `deploy/install.sh <device> --no-log-upload`.

## Test

```bash
make go-test
make go-vet
```

## Go runtime

The Go backend now covers the main runtime path: REST APIs, static UI serving,
Claude Desktop Computer Use transactions plus stream-json fallback sessions,
Claude/Codex native transcript readers, Codex app-server, Codex Desktop IPC sync,
WebSocket streaming, Web Push VAPID/subscription storage, encrypted push
delivery, foreground presence suppression, and approval-action callbacks.
The deploy installer builds and verifies the signed `bin/agenthalo`, installs an immutable runtime
copy at `/opt/private-tunnel/libexec/agenthalo/agenthalo`, and registers it with
private-services.

```bash
make go-test
make go-run     # http://127.0.0.1:18765
```

### Acceptance walkthrough

Claude Desktop-first provider, end-to-end:

1. `POST /sessions {provider_id: "claude", title: "..."}` creates a logical
   session whose sticky route is `desktop_computer_use`; capability preflight
   occurs before any Desktop mutation.
2. `POST /send_prompt` opens a bounded internal Computer Use / Locked Use
   transaction, verifies exact bundle/Team/session identity, obtains a fresh AX
   observation before each mutation, submits once, and synchronously closes and
   relocks. A result after an ambiguous mutation is `delivery_unknown`, never a
   CLI retry.
3. Side-effect-free observer hooks/transcripts update `/stream`, `/status`, and
   `/output` while the screen remains locked. Polling alone must not open a
   window.
4. A pending Claude permission or `AskUserQuestion` is keyed by session +
   request id. The human's `/approval` or `/question_answer` starts its own
   short transaction; allow selects only “Allow Once”, then closes/relocks.
5. Test a brand-new session with a deliberately failed capability preflight:
   it may bind once to `stream_json_cli`, after which SDK user frames,
   request-scoped control responses, output, and interrupt stay on that route.
6. Test that existing Desktop ownership, local input, exact-identity failure,
   and every post-mutation timeout refuse CLI replay.
7. Image blocks in Claude/Codex transcripts are returned as opaque asset ids;
   the PWA loads their bytes through the session-bound `/session_asset` route.
   Uploaded images follow the capabilities of the selected sticky route and are
   never silently rerouted.

The real target-Mac locked prompt, question, one-time allow/deny, terminal
close/relock, and no-duplicate-fallback walkthrough remains pending final E2E
acceptance; this document does not claim that `m4pro` has passed it.

> Codex login and account state are surfaced through the app-server provider; the
> agent never attempts to switch accounts or auto-answer human approval prompts.

Attaching a Claude Desktop session: `GET /native_sessions?provider_id=claude`
lists sessions from both CLI and Claude Desktop origins, deduped by transcript
uuid and tagged with `origin`. `POST /resume_native_session
{native_session_id}` persists the exact Desktop/native binding and keeps that
owner on `desktop_computer_use`. It must not terminate the Desktop owner or move
the session to CLI. Historical sessions already bound to `stream_json_cli`
remain on that route.

## Security

* Binds loopback or a private Unix domain socket; browser access is through
  private-tunnel mTLS and the agent socket's filesystem permissions.
* Account credentials / cookies / tokens / recovery codes are **never** logged.
* Upload ids are scoped to provider + logical session and never disclose the
  device path to the browser. A request cannot reuse another session's id.
* **Approval policy is provider-specific.** Claude tool permissions surface as
  request-scoped `waiting_approval`; the Desktop route may press only an exact
  one-time allow/deny control after a human decision. Always/session-wide
  permission, macOS passwords, TCC, Touch ID, account login, SSO, and MFA remain
  manual-only. CLI-fallback sessions are launched without bypass flags and use
  request-scoped SDK responses.
  Codex defaults to app-server
  `approval_policy=never` with `workspace-write` sandbox and can be tightened
  via config.

## Scaling design: many Macs · many providers · many sessions

```
 iPhone Web Console ──► Central Coordinator ──► private-tunnel (mTLS) ──► Agent (Mac A, device_id=A)
                                          └────────────────────────────► Agent (Mac B, device_id=B)
```

* **Per Mac**: one agent, one `device_id`, one or more providers.
  Account isolation stays at the device layer.
* **Central coordinator** (a small service, implementation TBD) aggregates agents:
  * **device registry** — `device_id` → base URL (private-tunnel path) + health.
  * **provider registry** — which providers each device exposes (proxied from
    each agent's `/providers`).
  * **session registry** — logical sessions keyed by
    `(device_id, provider_id, session_id)`; per-agent and aggregated.
  * **task registry** — all tasks across devices for a global activity view.
* **Heartbeat / stale**: each agent `POST /heartbeat`s the coordinator; a device
  with no heartbeat in a window is `offline`; per-task `stale`/`needs_manual`
  already exist in the agent and bubble up.
* **Approval workflow**: `waiting_approval` tasks fan in to one console queue;
  the human's response is routed by provider + session + request id to the
  owning Claude Desktop transaction or sticky CLI-fallback process, Codex
  app-server, or Codex Desktop owner.
* **Native task integration**: providers expose real sessions
  (`list_native_sessions`) and live output; a coordinator can poll per session
  for a multi-device task board.
* **Output-fidelity workers** plug into existing hooks: the Apple Vision OCR
  worker (`/ocr`) is available for screenshot analysis. Claude reads observer
  hook/transcript events (or structured stream events on its CLI fallback), and
  Codex reads app-server thread items plus notifications; neither needs OCR or
  clipboard for normal operation.

The phone selects **device → provider → session**, sends prompts, watches state,
and answers approvals — across the whole fleet.

## License

Released under the [MIT License](LICENSE).
