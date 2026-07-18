package provider

import (
	"encoding/json"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// codexDesktopBridge keeps one persistent follower connection to the Codex
// Desktop owner/follower IPC socket. The thread owner (a Codex Desktop /
// VS Code webview) broadcasts `thread-stream-state-changed` frames whose
// conversationState carries the raw pending app-server requests
// (`requests[]`, including approval requests with their JSON-RPC ids) plus
// the thread's real settings (approvalPolicy / approvalsReviewer /
// sandboxPolicy / model). The bridge mirrors that state so the provider can
// surface Desktop-owned approvals and answer them over
// `thread-follower-*-approval-decision` requests.
type codexDesktopBridge struct {
	socketPath string
	hostID     string
	dial       func(timeout time.Duration) (net.Conn, error)

	mu                     sync.Mutex
	threads                map[string]*codexBridgeThread
	connected              bool
	clientID               string
	conn                   net.Conn
	stopped                bool
	resyncAt               map[string]time.Time
	started                bool
	onHumanRequestsChanged func(threadID string)

	// resync issues thread-follower-load-complete-history so the owner
	// rebroadcasts a full snapshot after patch loss. Overridable in tests.
	resync func(threadID string, owner string)
}

type codexBridgeThread struct {
	id        string
	owner     string
	revision  int64
	state     map[string]any
	stale     bool
	updatedAt time.Time
}

const codexBridgeThreadTTL = 15 * time.Minute

func newCodexDesktopBridge(socketPath string, hostID string) *codexDesktopBridge {
	if hostID == "" {
		hostID = "local"
	}
	b := &codexDesktopBridge{
		socketPath: socketPath,
		hostID:     hostID,
		threads:    map[string]*codexBridgeThread{},
		resyncAt:   map[string]time.Time{},
	}
	b.resync = b.requestSnapshot
	return b
}

// Start launches the reconnect loop once.
func (b *codexDesktopBridge) Start() {
	b.mu.Lock()
	if b.started || b.stopped {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.mu.Unlock()
	go b.run()
}

func (b *codexDesktopBridge) Stop() {
	b.mu.Lock()
	b.stopped = true
	conn := b.conn
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (b *codexDesktopBridge) run() {
	backoff := time.Second
	for {
		b.mu.Lock()
		if b.stopped {
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
		if err := b.connectAndRead(); err == nil {
			backoff = time.Second
		}
		b.mu.Lock()
		stopped := b.stopped
		b.mu.Unlock()
		if stopped {
			return
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (b *codexDesktopBridge) dialConn(timeout time.Duration) (net.Conn, error) {
	if b.dial != nil {
		return b.dial(timeout)
	}
	path := b.socketPath
	if path == "" {
		path = defaultCodexDesktopSocket()
	}
	if path == "" {
		return nil, CodexDesktopIPCError{"Codex Desktop IPC socket not found"}
	}
	return net.DialTimeout("unix", path, timeout)
}

func (b *codexDesktopBridge) connectAndRead() error {
	conn, err := b.dialConn(5 * time.Second)
	if err != nil {
		return err
	}
	clientID, preInitFrames, err := codexBridgeInitialize(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	b.conn = conn
	b.connected = true
	b.clientID = clientID
	// Approval requests observed on a previous connection may have been
	// resolved while we were away; drop stream state and let the owners'
	// broadcasts repopulate it.
	b.threads = map[string]*codexBridgeThread{}
	b.mu.Unlock()
	// The router broadcasts our connected status before it returns the
	// initialize response. Existing Desktop owners react immediately by
	// replaying their thread snapshots, so those frames can arrive while
	// codexBridgeInitialize is still waiting. Preserve and apply them before
	// entering the steady-state read loop or the owner is lost until the next
	// conversation mutation.
	for _, frame := range preInitFrames {
		b.handleFrame(conn, frame)
	}
	err = b.readLoop(conn)
	b.mu.Lock()
	b.connected = false
	b.conn = nil
	// The connection is explicitly gone: pending Desktop approvals are no
	// longer answerable through this bridge, so hide them.
	b.threads = map[string]*codexBridgeThread{}
	b.mu.Unlock()
	_ = conn.Close()
	return err
}

func codexBridgeInitialize(conn net.Conn) (string, []map[string]any, error) {
	rid := newUUID()
	if err := writeDesktopFrame(conn, map[string]any{
		"type": "request", "requestId": rid, "sourceClientId": "initializing-client",
		"version": 0, "method": "initialize",
		"params": map[string]any{
			"clientType": "remote-agent",
			"clientInfo": map[string]any{"name": "remote-agent", "title": "remote-agent", "version": "0.0.1"},
		},
	}); err != nil {
		return "", nil, err
	}
	pending := []map[string]any{}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return "", nil, err
		}
		msg, err := readDesktopFrame(conn)
		if err != nil {
			return "", nil, err
		}
		switch stringAny(msg["type"]) {
		case "client-discovery-request":
			_ = writeDesktopFrame(conn, map[string]any{
				"type": "client-discovery-response", "requestId": stringAny(msg["requestId"]),
				"response": map[string]any{"canHandle": false},
			})
			continue
		}
		if stringAny(msg["requestId"]) == rid || stringAny(msg["id"]) == rid {
			res := mapAny(msg["result"])
			if len(res) == 0 {
				res = msg
			}
			cid := firstNonEmpty(stringAny(res["clientId"]), stringAny(res["client_id"]))
			if cid == "" {
				cid = "remote-agent"
			}
			return cid, pending, nil
		}
		pending = append(pending, msg)
	}
}

func (b *codexDesktopBridge) readLoop(conn net.Conn) error {
	for {
		if err := conn.SetReadDeadline(time.Now().Add(120 * time.Second)); err != nil {
			return err
		}
		msg, err := readDesktopFrame(conn)
		if err != nil {
			// Idle timeout is fine: keep listening as long as the socket
			// stays open; other errors abort and trigger reconnect.
			if strings.Contains(err.Error(), "timeout") {
				continue
			}
			return err
		}
		b.handleFrame(conn, msg)
	}
}

func (b *codexDesktopBridge) handleFrame(conn net.Conn, msg map[string]any) {
	switch stringAny(msg["type"]) {
	case "client-discovery-request":
		_ = writeDesktopFrame(conn, map[string]any{
			"type": "client-discovery-response", "requestId": stringAny(msg["requestId"]),
			"response": map[string]any{"canHandle": false},
		})
	case "request":
		_ = writeDesktopFrame(conn, map[string]any{
			"type": "response", "requestId": stringAny(msg["requestId"]),
			"resultType": "error", "error": "no-handler-for-request",
		})
	case "broadcast":
		if stringAny(msg["method"]) == "thread-stream-state-changed" {
			b.handleStreamStateChanged(msg)
		}
	}
}

// HandleBroadcast feeds one already-decoded broadcast frame into the bridge
// state. Exposed for tests.
func (b *codexDesktopBridge) HandleBroadcast(msg map[string]any) {
	if stringAny(msg["type"]) == "broadcast" && stringAny(msg["method"]) == "thread-stream-state-changed" {
		b.handleStreamStateChanged(msg)
	}
}

func (b *codexDesktopBridge) handleStreamStateChanged(msg map[string]any) {
	params := mapAny(msg["params"])
	threadID := firstNonEmpty(stringAny(params["conversationId"]), stringAny(params["threadId"]))
	if threadID == "" {
		return
	}
	owner := stringAny(msg["sourceClientId"])
	change := mapAny(params["change"])
	changeType := stringAny(change["type"])
	now := time.Now()

	b.mu.Lock()
	b.pruneLocked(now)
	th := b.threads[threadID]
	oldRequests := ""
	if th != nil && !th.stale {
		oldRequests = codexHumanRequestsSignature(th.state)
	}
	switch changeType {
	case "snapshot":
		state := mapAny(change["conversationState"])
		if len(state) == 0 {
			b.mu.Unlock()
			return
		}
		b.threads[threadID] = &codexBridgeThread{
			id: threadID, owner: owner, revision: int64Any(change["revision"]),
			state: state, updatedAt: now,
		}
		changed := oldRequests != codexHumanRequestsSignature(state)
		notify := b.onHumanRequestsChanged
		b.mu.Unlock()
		if changed && notify != nil {
			notify(threadID)
		}
		return
	case "patches":
		if th == nil || th.stale || th.owner != owner || th.revision != int64Any(change["baseRevision"]) {
			if th != nil {
				th.stale = true
				th.owner = owner
				th.updatedAt = now
			} else {
				b.threads[threadID] = &codexBridgeThread{id: threadID, owner: owner, stale: true, updatedAt: now}
			}
			b.mu.Unlock()
			b.maybeResync(threadID, owner)
			return
		}
		next, err := applyImmerPatches(th.state, listAny(change["patches"]))
		if err != nil {
			th.stale = true
			th.updatedAt = now
			b.mu.Unlock()
			b.maybeResync(threadID, owner)
			return
		}
		th.state = next
		th.revision = int64Any(change["revision"])
		th.updatedAt = now
		changed := oldRequests != codexHumanRequestsSignature(next)
		notify := b.onHumanRequestsChanged
		b.mu.Unlock()
		if changed && notify != nil {
			notify(threadID)
		}
		return
	}
	b.mu.Unlock()
}

func (b *codexDesktopBridge) pruneLocked(now time.Time) {
	for id, th := range b.threads {
		if now.Sub(th.updatedAt) > codexBridgeThreadTTL {
			delete(b.threads, id)
		}
	}
}

func (b *codexDesktopBridge) maybeResync(threadID string, owner string) {
	b.mu.Lock()
	last := b.resyncAt[threadID]
	if time.Since(last) < 5*time.Second {
		b.mu.Unlock()
		return
	}
	b.resyncAt[threadID] = time.Now()
	resync := b.resync
	b.mu.Unlock()
	if resync != nil {
		go resync(threadID, owner)
	}
}

func (b *codexDesktopBridge) requestSnapshot(threadID string, owner string) {
	client := &CodexDesktopIPCClient{SocketPath: b.socketPath, Timeout: 8 * time.Second, ClientType: "remote-agent", HostID: b.hostID, clientID: "initializing-client"}
	_, _ = client.request("thread-follower-load-complete-history", map[string]any{
		"hostId": b.hostID, "conversationId": threadID,
	}, 8*time.Second, owner)
}

// RefreshThread asks the Desktop owner to rebroadcast a complete conversation
// snapshot and briefly waits for the persistent bridge to observe it. This is
// required when remote-agent attaches after an approval/question was already
// created: incremental broadcasts alone cannot reconstruct the old request.
func (b *codexDesktopBridge) RefreshThread(threadID string, owner string, timeout time.Duration) bool {
	if threadID == "" {
		return false
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	b.mu.Lock()
	if owner == "" {
		if th := b.threads[threadID]; th != nil {
			owner = th.owner
		}
	}
	baseline := time.Time{}
	if th := b.threads[threadID]; th != nil {
		baseline = th.updatedAt
	}
	resync := b.resync
	b.mu.Unlock()
	if owner == "" || resync == nil {
		return false
	}

	done := make(chan struct{})
	go func() {
		resync(threadID, owner)
		close(done)
	}()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	resyncDone := false
	doneC := (<-chan struct{})(done)
	var grace *time.Timer
	var graceC <-chan time.Time
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	fresh := func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		th := b.threads[threadID]
		return th != nil && !th.stale && th.state != nil && th.updatedAt.After(baseline)
	}
	for {
		if fresh() {
			return true
		}
		select {
		case <-doneC:
			if !resyncDone {
				resyncDone = true
				doneC = nil
				grace = time.NewTimer(150 * time.Millisecond)
				graceC = grace.C
			}
		case <-ticker.C:
		case <-graceC:
			return fresh()
		case <-deadline.C:
			return fresh()
		}
	}
}

func (b *codexDesktopBridge) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

// OwnerClient returns the client id currently broadcasting this thread.
func (b *codexDesktopBridge) OwnerClient(threadID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if th := b.threads[threadID]; th != nil {
		return th.owner
	}
	return ""
}

func (b *codexDesktopBridge) HasThread(threadID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	th := b.threads[threadID]
	return th != nil && !th.stale && th.state != nil
}

// codexBridgeRequest is one pending human request mirrored from the owner's
// conversation state.
type codexBridgeRequest struct {
	ID     any
	Method string
	Params map[string]any
}

// PendingHumanRequests lists the thread's outstanding approval/input server
// requests, oldest first. Machine requests (dynamic tools, token refresh,
// attestation, clocks) are excluded.
func (b *codexDesktopBridge) PendingHumanRequests(threadID string) []codexBridgeRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	th := b.threads[threadID]
	if th == nil || th.stale || th.state == nil {
		return nil
	}
	out := []codexBridgeRequest{}
	for _, raw := range listAny(th.state["requests"]) {
		req := mapAny(raw)
		method := stringAny(req["method"])
		if !codexHumanRequestMethod(method) {
			continue
		}
		out = append(out, codexBridgeRequest{ID: req["id"], Method: method, Params: mapAny(req["params"])})
	}
	return out
}

// ThreadSettings reports the thread's real effective settings as owned by
// Desktop (approval policy, reviewer, sandbox, model, effort).
func (b *codexDesktopBridge) ThreadSettings(threadID string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	th := b.threads[threadID]
	if th == nil || th.state == nil {
		return nil
	}
	settings := mapAny(th.state["latestThreadSettings"])
	perms := mapAny(th.state["currentPermissions"])
	pick := func(key string) any {
		if v, ok := perms[key]; ok && v != nil {
			return v
		}
		return settings[key]
	}
	out := map[string]any{}
	if v := pick("approvalPolicy"); v != nil {
		out["approval_policy"] = v
	}
	if v := pick("approvalsReviewer"); v != nil {
		out["approvals_reviewer"] = v
	}
	if v := pick("sandboxPolicy"); v != nil {
		out["sandbox_policy"] = v
		if mode := codexSandboxModeName(mapAny(v)); mode != "" {
			out["sandbox"] = mode
		}
	}
	if v := stringAny(settings["model"]); v != "" {
		out["model"] = v
	}
	if v := firstNonEmpty(stringAny(settings["effort"]), stringAny(settings["reasoningEffort"])); v != "" {
		out["effort"] = v
	}
	if len(out) == 0 {
		return nil
	}
	out["source"] = "codex_desktop_ipc"
	return out
}

// ThreadRunning reports whether the Desktop owner considers the thread's
// runtime active. nil when the bridge has no state for the thread.
func (b *codexDesktopBridge) ThreadRunning(threadID string) *bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	th := b.threads[threadID]
	if th == nil || th.state == nil {
		return nil
	}
	running := codexBridgeStateRunning(th.state)
	return &running
}

func codexBridgeStateRunning(state map[string]any) bool {
	rt := mapAny(state["threadRuntimeStatus"])
	if t := stringAny(rt["type"]); t != "" {
		return t != "idle"
	}
	for _, turn := range mapsFromAny(state["turns"]) {
		if desktopTurnStatusLive(stringAny(turn["status"])) {
			return true
		}
	}
	return false
}

// LiveThreadRows converts bridge state to the runtime-session row shape used
// by desktopSnapshotLiveRows so callers can swap sources transparently.
func (b *codexDesktopBridge) LiveThreadRows() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := make([]map[string]any, 0, len(b.threads))
	for _, th := range b.threads {
		if th.state == nil {
			continue
		}
		running := codexBridgeStateRunning(th.state)
		status := "idle"
		st := "idle"
		if running {
			status = "active"
			st = "running"
		}
		row := map[string]any{
			"session_id":        th.id,
			"provider_id":       "codex",
			"native_session_id": th.id,
			"transcript_id":     th.id,
			"codex_thread_id":   th.id,
			"title":             desktopConversationTitle(th.state, th.id),
			"cwd":               desktopConversationCwd(th.state),
			"live":              running,
			"state":             st,
			"status":            status,
			"updated_at":        th.updatedAt.UTC().Format(time.RFC3339Nano),
		}
		if th.owner != "" {
			row["desktop_owner_client_id"] = th.owner
		}
		rows = append(rows, row)
	}
	sortByUpdated(rows)
	return rows
}

func codexSandboxModeName(policy map[string]any) string {
	switch stringAny(policy["type"]) {
	case "dangerFullAccess":
		return "danger-full-access"
	case "readOnly":
		return "read-only"
	case "workspaceWrite":
		return "workspace-write"
	}
	return ""
}

// codexHumanRequestMethod reports whether an app-server server->client
// request genuinely needs a human decision. Everything else (dynamic tool
// calls, token refresh, attestation, clock reads, fuzzy search plumbing)
// must never surface as an approval.
func codexHumanRequestMethod(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"item/tool/requestUserInput",
		"mcpServer/elicitation/request",
		"execCommandApproval",
		"applyPatchApproval":
		return true
	}
	if strings.HasSuffix(method, "/requestApproval") || strings.HasSuffix(method, "/requestUserInput") {
		return true
	}
	if strings.Contains(strings.ToLower(method), "elicitation") && strings.HasSuffix(method, "/request") {
		return true
	}
	if strings.HasSuffix(method, "Approval") && !strings.Contains(method, "/") {
		return true
	}
	return false
}

func codexHumanRequestsSignature(state map[string]any) string {
	if state == nil {
		return ""
	}
	parts := []string{}
	for _, raw := range listAny(state["requests"]) {
		req := mapAny(raw)
		method := stringAny(req["method"])
		if !codexHumanRequestMethod(method) {
			continue
		}
		parts = append(parts, canonicalRequestKey(req["id"])+"\x1f"+method)
	}
	return strings.Join(parts, "\x1e")
}

// --- immer patch application -------------------------------------------------

// applyImmerPatches applies immer-style patches ({op, path[], value}) to a
// JSON object tree, returning a new root. Array paths use numeric segments;
// a trailing "length" segment with op=replace resizes the array.
func applyImmerPatches(root map[string]any, patches []any) (map[string]any, error) {
	current := any(deepCopyJSON(root))
	for _, raw := range patches {
		patch := mapAny(raw)
		op := stringAny(patch["op"])
		path := listAny(patch["path"])
		if len(path) == 0 {
			if op == "replace" {
				current = deepCopyJSON(patch["value"])
				continue
			}
			return nil, CodexDesktopIPCError{"immer patch: empty path for op " + op}
		}
		next, err := applyImmerPatch(current, path, op, patch["value"])
		if err != nil {
			return nil, err
		}
		current = next
	}
	out, ok := current.(map[string]any)
	if !ok {
		return nil, CodexDesktopIPCError{"immer patch: root is not an object"}
	}
	return out, nil
}

func applyImmerPatch(node any, path []any, op string, value any) (any, error) {
	key := path[0]
	if len(path) == 1 {
		return applyImmerLeaf(node, key, op, value)
	}
	switch container := node.(type) {
	case map[string]any:
		k := stringAny(key)
		child, ok := container[k]
		if !ok {
			return nil, CodexDesktopIPCError{"immer patch: missing key " + k}
		}
		next, err := applyImmerPatch(child, path[1:], op, value)
		if err != nil {
			return nil, err
		}
		container[k] = next
		return container, nil
	case []any:
		idx, ok := immerIndex(key)
		if !ok || idx < 0 || idx >= len(container) {
			return nil, CodexDesktopIPCError{"immer patch: bad array index"}
		}
		next, err := applyImmerPatch(container[idx], path[1:], op, value)
		if err != nil {
			return nil, err
		}
		container[idx] = next
		return container, nil
	}
	return nil, CodexDesktopIPCError{"immer patch: cannot descend into scalar"}
}

func applyImmerLeaf(node any, key any, op string, value any) (any, error) {
	switch container := node.(type) {
	case map[string]any:
		k := stringAny(key)
		switch op {
		case "add", "replace":
			container[k] = deepCopyJSON(value)
		case "remove":
			delete(container, k)
		default:
			return nil, CodexDesktopIPCError{"immer patch: unknown op " + op}
		}
		return container, nil
	case []any:
		if s := stringAny(key); s == "length" && op == "replace" {
			n, ok := immerIndex(value)
			if !ok || n < 0 {
				return nil, CodexDesktopIPCError{"immer patch: bad length"}
			}
			if n <= len(container) {
				return container[:n], nil
			}
			for len(container) < n {
				container = append(container, nil)
			}
			return container, nil
		}
		idx, ok := immerIndex(key)
		if !ok || idx < 0 {
			return nil, CodexDesktopIPCError{"immer patch: bad array index"}
		}
		switch op {
		case "replace":
			if idx >= len(container) {
				return nil, CodexDesktopIPCError{"immer patch: replace out of range"}
			}
			container[idx] = deepCopyJSON(value)
			return container, nil
		case "add":
			if idx > len(container) {
				return nil, CodexDesktopIPCError{"immer patch: add out of range"}
			}
			container = append(container, nil)
			copy(container[idx+1:], container[idx:])
			container[idx] = deepCopyJSON(value)
			return container, nil
		case "remove":
			if idx >= len(container) {
				return nil, CodexDesktopIPCError{"immer patch: remove out of range"}
			}
			return append(container[:idx], container[idx+1:]...), nil
		}
		return nil, CodexDesktopIPCError{"immer patch: unknown op " + op}
	}
	return nil, CodexDesktopIPCError{"immer patch: leaf parent is scalar"}
}

func immerIndex(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyJSON(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyJSON(vv)
		}
		return out
	default:
		return v
	}
}

func int64Any(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
