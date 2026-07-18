# AI Desktop Remote Agent (MVP)

The repository and Go module are named `remote-agent`. The installed command,
supervisor service, state paths, and browser storage keys remain
`remote-agent` for backward compatibility with existing deployments.

A small **macOS local agent** that lets you drive AI coding/chat agents on a Mac
from a phone/browser — **without** RDP, VNC, Parsec or any remote-desktop/video
protocol.

Each Mac logs into one isolated AI account and runs one agent. The agent feeds
tasks to the AI app and reads the output back, through a **provider/adapter**
layer:

| Family | Providers | How it drives the agent | How it reads output |
|--------|-----------|-------------------------|---------------------|
| **Claude** | `claude` | creates/resumes the real CLI as a managed `stream-json` child | live NDJSON events + merged Desktop/CLI transcript metadata |
| **Codex Desktop/app-server** | `codex` | maps each logical session to a Codex thread and binds it to either Desktop owner/follower IPC or its own headless `codex app-server` (`turn/start`, `turn/steer`, `turn/interrupt`) | merged app-server/local discovery, app-server notifications for headless turns, and rollout preview tailing for Desktop-owned turns |

> **Account isolation is solved at the device layer.** One Mac ≈ one Claude
> account or one Codex account. The agent never switches or logs into accounts.

Claude exposes one provider and one identity namespace. Desktop session metadata
and CLI transcript metadata are merged by Claude transcript id, while all input,
streaming, approval, question, and interrupt control uses the managed CLI
`stream-json` process. The legacy `claude_cli` and `claude_desktop` ids resolve
to `claude`; they are not separate owners.

All provider paths are intended for real coding work: Claude uses its documented
stdin/stdout `stream-json` protocol, and Codex uses app-server because it matches
Codex Desktop / VS Code's thread-turn-item model and exposes native streaming,
steer, and interrupt operations.

```
remote-agent/
├── go.mod
├── Makefile
├── README.md
├── cmd/remote-agent/           # Go service entrypoint
├── internal/                    # Go API/config/state/provider implementation
├── config.example.json          # copy to config.json and edit
├── data/                         # local runtime state (ignored by Git)
├── static/
│   ├── shell.html               # stable relay device picker
│   └── index.html               # full console embedded into the agent binary
├── scripts/
│   └── ocr_vision.swift         # local Apple Vision OCR worker
├── deploy/
│   └── private-tunnel.example.yaml   # how to expose the agent via ../private-tunnel
└── screenshots/
    └── .gitkeep
```

## Architecture: provider / adapter

The production agent is the Go binary `bin/remote-agent`. Its registry exposes
canonical `claude` and `codex` providers: Claude is a stream-json CLI provider
with merged Desktop/CLI discovery, and Codex binds each logical session to
either an app-server or Desktop-IPC delivery route.

The complete current model — provider contract, logical/native identity,
discovery/runtime merge, delivery ownership, streaming and approvals — is in
[`docs/provider-architecture.md`](docs/provider-architecture.md).

The Go `Provider` interface owns status/model metadata, native discovery and
preview, logical session lifecycle, prompt/output/state, approvals, interrupt
and model selection. Small optional interfaces add attachments, transcript
assets, runtime sessions, native resume/fork, precise request-scoped approvals,
Desktop delivery binding and Codex message rewind without bloating the base
contract.

### claude provider — stream-json

* **One logical session → one official CLI child process**. New sessions use
  `--session-id`; existing transcripts use `--resume`. The Desktop-managed
  signed binary may be executed directly, but its bundle is never modified.
* **Input**: one complete SDK `user` NDJSON frame is written to stdin. Multiline
  and non-ASCII prompts remain structured JSON rather than terminal keystrokes.
* **Output**: `--output-format stream-json` publishes assistant deltas,
  thinking/tool items, tool results, hook events, and turn completion directly
  to the existing WebSocket stream. The transcript remains the durable read
  side after a service restart.
* **Approval/questions**: request-scoped `control_request` frames are surfaced
  to the PWA; allow/deny and `AskUserQuestion` answers return structured
  `control_response` frames. No bypass flags or automatic approvals are used.
* **Interrupt**: sends the SDK `control_request{subtype:"interrupt"}` instead
  of simulating Escape. A second prompt is rejected while the session owns a
  running turn.
* **Merged discovery**: `~/.claude/projects` transcripts and Claude Desktop's
  `claude-code-sessions` metadata are joined by `cliSessionId`. Desktop titles,
  cwd, and timestamps enrich the same PWA row; selecting it resumes through the
  CLI and never routes input to Desktop IPC.
* **Single-owner Desktop handoff**: before resuming a Desktop-origin transcript,
  the provider finds the internal Desktop CLI by an exact session alias or its
  open transcript file, terminates only that CLI process family, and confirms
  exit before starting standalone CLI. If ownership or exit cannot be safely
  confirmed, resume fails instead of creating a competing writer.
* **Pre-existing questions remain visible**: an unanswered transcript-only
  `AskUserQuestion` is shown as non-actionable when its original stdio callback
  is gone. The PWA does not pretend a new CLI owner can answer an old request;
  live managed-CLI questions remain fully answerable.
* **Per-turn usage**: the transcript preview appends a local annotation after
  every completed turn with input/output/cache-create/cache-read tokens,
  wall-clock duration, and a standard API-price estimate in USD. Repeated
  Claude transcript records for the same streamed API message are deduplicated.

### codex provider — app-server

The registered `codex` provider is the Go `provider.Codex`.

* **Desktop owner first for attached threads**. A logical web session maps to a
  native Codex thread id. When that session was attached to an existing Codex
  UUID thread, remote-agent first asks Codex Desktop over the same-user
  owner/follower IPC socket to start/steer/interrupt the turn, so Desktop renders
  the turn live instead of catching up only after refresh. Attach also requests
  a complete Desktop snapshot so approvals or user-input questions that were
  already pending before the web session connected are restored immediately.
* **Delivery route belongs to the logical session**. New remote-agent-created
  threads are headless app-server sessions. Sending from a native Codex preview
  persists `delivery_route=desktop_ipc`, opens the thread in Desktop if needed,
  and targets its owner client. A Desktop-routed session never falls back to a
  second app-server owner when attach/IPC fails; the request returns an error.
* **Native reads**: `/native_sessions` merges app-server `thread/list` with the
  local Codex index/rollouts. `/session_preview` reads rollout JSONL first and
  only uses a bounded `thread/resume` fallback when the rollout is not yet on
  disk.
* **Live output**: headless turns publish app-server `item/agentMessage/delta`
  and `thread/status/changed` notifications to both the logical session id and
  native thread id. The PWA live-tails Desktop-owned turns by polling
  `/session_preview`; the Desktop follower bridge supplies owner/running/settings
  and pending-request changes.
* **Approvals bridge over Desktop IPC**. Approval requests for a Desktop-owned
  turn are sent by app-server to the turn's *owner* client, not to
  remote-agent's own app-server child. A persistent follower connection
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
* **Binary selection**: by default it prefers
  `/Applications/Codex.app/Contents/Resources/codex` (or the same under
  `~/Applications`) and falls back to `$PATH`; set
  `extra.prefer_desktop_codex=false` to opt out.
* **Per-turn usage**: completed `task_started` / `task_complete` boundaries use
  Codex's native duration and the delta of cumulative token counters, so a
  session total is never repeated as an individual turn's usage.

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
| GET  | `/providers` | all providers + ProviderStatus + capabilities |
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

## Setup

### 1. Install

```bash
cd remote-agent
make go-build
cp config.example.json config.json       # edit device_id, cwd, providers
```

Requires a runnable standalone `claude` CLI for the `claude` provider. Codex prefers the
binary bundled inside Codex.app and only falls back to PATH:

```bash
which claude
test -x /Applications/Codex.app/Contents/Resources/codex || which codex
```

### 2. macOS permissions

| Permission | Needed by | Where |
|------------|-----------|-------|
| **Screen Recording** | `screencapture` (screenshots, the OCR input) | System Settings → Privacy & Security → Screen Recording |

The Claude stream-json path and Codex app-server path need **none** of these for core
operation. Screen Recording is only needed if you use the `/screenshot` or
`/ocr` endpoints.

### 3. Run

```bash
./bin/remote-agent --config config.json
# -> unix socket from config.json, or http://127.0.0.1:8765 when no uds is set
```

Open `http://127.0.0.1:8765`, pick a provider, create a session, and send a
prompt.

### 4. Reach it from iPhone — via private-tunnel (no public ports)

The agent binds loopback only. Expose it through a private-tunnel-compatible
reverse tunnel (mTLS). Two config edits —
see [deploy/private-tunnel.example.yaml](deploy/private-tunnel.example.yaml):

1. **Relay**: add a `remotecoding` service with `port: 8765` and
   `default_device: <this-mac>` (no `static_dir` — the agent serves its own UI).
   `default_device` makes the relay strip the `/s/remotecoding/` prefix and
   forward clean paths to the agent, so the UI's absolute paths work.
2. **private-edge profile**: keep port `8765` mapped through the dedicated
   vmnet UDS gateway. Do not install or restart a host tunnel agent.

Then on the phone (mTLS client cert installed):
`https://<user>-relay.<domain>/s/remotecoding/`.

The service root serves a relay-owned device host. It keeps the root PWA URL,
chooses the last device from browser storage (or the most recently connected
available device), and loads `/s/remotecoding/d/<device>/` inside a same-origin
frame. The embedded console keeps every tab bound to its own device, so session
switches route status, output, input, approvals, and WebSocket traffic to that
session's agent. Normal UI/backend releases therefore update devices through
the release manifest without rewriting the relay PWA shell.

The security boundary is the local UDS/filesystem permission plus
private-tunnel **mTLS**. Never expose port 8765 directly.

Auto-update and log upload are opt-in in the public repository. Set
`RC_UPDATE_RELAY_URL` to enable manifest polling. Pass `--log-relay-url` (or
set `RC_LOG_UPLOAD_RELAY_URL`) when installing with log upload, or use
`deploy/install.sh <device> --no-log-upload`.

## Test

```bash
make go-test
make go-vet
```

## Go runtime

The Go backend now covers the main runtime path: REST APIs, static UI serving,
Claude stream-json sessions, Claude/Codex native transcript readers, Codex app-server,
Codex Desktop IPC sync, WebSocket streaming, Web Push VAPID/subscription storage,
encrypted push delivery, foreground presence suppression, and approval-action
callbacks.
The deploy installer builds `bin/remote-agent`, installs an immutable runtime
copy at `/opt/private-tunnel/libexec/remote-agent/remote-agent`, and registers it with
private-services.

```bash
make go-test
make go-run     # http://127.0.0.1:18765
```

### Acceptance walkthrough

Claude stream-json provider, end-to-end:

1. `POST /sessions {provider_id: "claude", title: "..."}` starts the real
   CLI with bidirectional NDJSON pipes.
2. `POST /send_prompt` writes one SDK `user` frame; `/stream` receives assistant,
   thinking, tool/result, and turn lifecycle frames.
3. A permission `control_request` makes `/status` report
   `waiting_approval`; `/approval` returns the matching `control_response`.
4. `AskUserQuestion` uses `/question_answer`, scoped by session and request id.
5. A second prompt while running is rejected; `/interrupt` sends a structured
   interrupt request and returns the session to idle.
6. `GET /output` returns `{source:"claude_cli_stream", ...}` and transcript
   preview remains available after the child exits.
7. Image blocks in Claude/Codex transcripts are returned as opaque asset ids;
   the PWA loads their bytes through the session-bound `/session_asset` route.
   Uploaded images are native multimodal inputs (`image` for Claude,
   `localImage` for Codex); other uploaded files are passed as private local
   paths visible only to the selected agent session.

> Codex login and account state are surfaced through the app-server provider; the
> agent never attempts to switch accounts or auto-answer human approval prompts.

Attaching a Claude Desktop session: `GET /native_sessions?provider_id=claude`
lists sessions from both CLI and Claude Desktop origins, deduped by transcript
uuid and tagged with `origin`. `POST /resume_native_session
{native_session_id}` first hands off any matching Claude Desktop internal CLI
owner, then resumes that transcript in the managed stream-json CLI process.
Non-Desktop owners must still become idle before resume.

## Security

* Binds loopback or a private Unix domain socket; browser access is through
  private-tunnel mTLS and the agent socket's filesystem permissions.
* Account credentials / cookies / tokens / recovery codes are **never** logged.
* Upload ids are scoped to provider + logical session and never disclose the
  device path to the browser. A request cannot reuse another session's id.
* **Approval policy is provider-specific.** Claude is launched without bypass
  flags, and risky/login prompts surface as `waiting_approval` /
  `needs_manual`. Stream sessions use request-scoped SDK responses.
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
  owning Claude stream-json process, Codex app-server, or Codex Desktop owner.
* **Native task integration**: providers expose real sessions
  (`list_native_sessions`) and live output; a coordinator can poll per session
  for a multi-device task board.
* **Output-fidelity workers** plug into existing hooks: the Apple Vision OCR
  worker (`/ocr`) is available for screenshot analysis. Claude reads structured
  stream events plus transcript, and Codex reads app-server thread items plus notifications;
  neither needs OCR or clipboard for normal operation.

The phone selects **device → provider → session**, sends prompts, watches state,
and answers approvals — across the whole fleet.

## License

Released under the [MIT License](LICENSE).
