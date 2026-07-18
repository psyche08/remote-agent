package provider

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type CodexDesktopIPCError struct {
	Message string
}

func (e CodexDesktopIPCError) Error() string { return e.Message }

type codexDesktopClient interface {
	StartTurn(conversationID string, prompt string, opts map[string]any, timeout time.Duration) (any, error)
	SteerTurn(conversationID string, prompt string, timeout time.Duration) (any, error)
	InterruptTurn(conversationID string, timeout time.Duration) (any, error)
}

type CodexDesktopIPCClient struct {
	SocketPath string
	Timeout    time.Duration
	ClientType string
	HostID     string
	clientID   string
}

func NewCodexDesktopIPCClient(socketPath string, timeout time.Duration, hostID string) *CodexDesktopIPCClient {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if hostID == "" {
		hostID = "local"
	}
	return &CodexDesktopIPCClient{SocketPath: socketPath, Timeout: timeout, ClientType: "remote-agent", HostID: hostID, clientID: "initializing-client"}
}

func (c *CodexDesktopIPCClient) StartTurn(conversationID string, prompt string, opts map[string]any, timeout time.Duration) (any, error) {
	return c.StartTurnOnClient(conversationID, prompt, opts, "", timeout)
}

func (c *CodexDesktopIPCClient) StartTurnWithAttachments(conversationID string, prompt string, attachments []Attachment, opts map[string]any, timeout time.Duration) (any, error) {
	return c.StartTurnOnClientWithAttachments(conversationID, prompt, attachments, opts, "", timeout)
}

func (c *CodexDesktopIPCClient) StartTurnOnClient(conversationID string, prompt string, opts map[string]any, targetClientID string, timeout time.Duration) (any, error) {
	return c.StartTurnOnClientWithAttachments(conversationID, prompt, nil, opts, targetClientID, timeout)
}

func (c *CodexDesktopIPCClient) StartTurnOnClientWithAttachments(conversationID string, prompt string, attachments []Attachment, opts map[string]any, targetClientID string, timeout time.Duration) (any, error) {
	turnStart := compactMap(map[string]any{
		"input":           codexUserInput(prompt, attachments),
		"cwd":             opts["cwd"],
		"approvalPolicy":  firstValue(opts["approval_policy"], opts["approvalPolicy"]),
		"sandbox":         opts["sandbox"],
		"model":           opts["model"],
		"reasoningEffort": opts["reasoningEffort"],
		"serviceTier":     opts["serviceTier"],
	})
	return c.request("thread-follower-start-turn", map[string]any{
		"hostId":          c.HostID,
		"conversationId":  conversationID,
		"turnStartParams": turnStart,
	}, timeout, targetClientID)
}

func (c *CodexDesktopIPCClient) SteerTurn(conversationID string, prompt string, timeout time.Duration) (any, error) {
	return c.SteerTurnOnClient(conversationID, prompt, "", timeout)
}

func (c *CodexDesktopIPCClient) SteerTurnOnClient(conversationID string, prompt string, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-steer-turn", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID, "input": inputText(prompt),
		"restoreMessage": nil, "attachments": []any{},
	}, timeout, targetClientID)
}

func (c *CodexDesktopIPCClient) InterruptTurn(conversationID string, timeout time.Duration) (any, error) {
	return c.InterruptTurnOnClient(conversationID, "", timeout)
}

func (c *CodexDesktopIPCClient) InterruptTurnOnClient(conversationID string, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-interrupt-turn", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID,
	}, timeout, targetClientID)
}

// CommandApprovalDecision relays a decision for a pending
// item/commandExecution/requestApproval owned by a Desktop client. requestID
// is the owner's app-server JSON-RPC request id; decision is a
// CommandExecutionApprovalDecision value (e.g. "accept", "decline", "cancel").
func (c *CodexDesktopIPCClient) CommandApprovalDecision(conversationID string, requestID any, decision any, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-command-approval-decision", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID, "requestId": requestID, "decision": decision,
	}, timeout, targetClientID)
}

// FileApprovalDecision relays a decision for a pending
// item/fileChange/requestApproval owned by a Desktop client.
func (c *CodexDesktopIPCClient) FileApprovalDecision(conversationID string, requestID any, decision any, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-file-approval-decision", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID, "requestId": requestID, "decision": decision,
	}, timeout, targetClientID)
}

// PermissionsApprovalResponse relays a full PermissionsRequestApprovalResponse
// body for a pending item/permissions/requestApproval.
func (c *CodexDesktopIPCClient) PermissionsApprovalResponse(conversationID string, requestID any, response map[string]any, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-permissions-request-approval-response", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID, "requestId": requestID, "response": response,
	}, timeout, targetClientID)
}

// SubmitUserInput relays a ToolRequestUserInputResponse body for a pending
// item/tool/requestUserInput.
func (c *CodexDesktopIPCClient) SubmitUserInput(conversationID string, requestID any, response map[string]any, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-submit-user-input", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID, "requestId": requestID, "response": response,
	}, timeout, targetClientID)
}

// SubmitMcpElicitationResponse relays an McpServerElicitationRequestResponse
// body for a pending mcpServer/elicitation/request.
func (c *CodexDesktopIPCClient) SubmitMcpElicitationResponse(conversationID string, requestID any, response map[string]any, targetClientID string, timeout time.Duration) (any, error) {
	return c.request("thread-follower-submit-mcp-server-elicitation-response", map[string]any{
		"hostId": c.HostID, "conversationId": conversationID, "requestId": requestID, "response": response,
	}, timeout, targetClientID)
}

func (c *CodexDesktopIPCClient) SnapshotLiveThreads(timeout time.Duration) []map[string]any {
	if timeout <= 0 {
		timeout = c.Timeout
	}
	conn, err := c.connect(timeout)
	if err != nil {
		return nil
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if err := c.initialize(conn, timeout); err != nil {
		return nil
	}
	byThread := map[string]map[string]any{}
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(deadline); err != nil {
			break
		}
		msg, err := readDesktopFrame(conn)
		if err != nil {
			break
		}
		for _, row := range desktopSnapshotLiveRows(msg) {
			tid := stringAny(row["transcript_id"])
			if tid != "" {
				byThread[tid] = row
			}
		}
	}
	rows := make([]map[string]any, 0, len(byThread))
	for _, row := range byThread {
		rows = append(rows, row)
	}
	sortByUpdated(rows)
	return rows
}

func (c *CodexDesktopIPCClient) request(method string, params map[string]any, timeout time.Duration, targetClientID string) (any, error) {
	if timeout <= 0 {
		timeout = c.Timeout
	}
	conn, err := c.connect(timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := c.initialize(conn, timeout); err != nil {
		return nil, err
	}
	return c.requestOnConn(conn, method, params, timeout, "", targetClientID)
}

func (c *CodexDesktopIPCClient) connect(timeout time.Duration) (net.Conn, error) {
	path := c.SocketPath
	if path == "" {
		path = defaultCodexDesktopSocket()
	}
	if path == "" {
		return nil, CodexDesktopIPCError{"Codex Desktop IPC socket not found"}
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, CodexDesktopIPCError{"failed to connect Codex Desktop IPC socket " + path + ": " + err.Error()}
	}
	return conn, nil
}

func (c *CodexDesktopIPCClient) initialize(conn net.Conn, timeout time.Duration) error {
	res, err := c.requestOnConn(conn, "initialize", map[string]any{
		"clientType": c.ClientType,
		"clientInfo": map[string]any{"name": c.ClientType, "title": c.ClientType, "version": "0.0.1"},
	}, timeout, "initializing-client", "")
	if err != nil {
		return err
	}
	if m := mapAny(res); len(m) > 0 {
		cid := firstNonEmpty(stringAny(m["clientId"]), stringAny(m["client_id"]))
		if cid == "" {
			cid = firstNonEmpty(stringAny(mapAny(m["result"])["clientId"]), stringAny(mapAny(m["result"])["client_id"]))
		}
		if cid != "" {
			c.clientID = cid
		}
	}
	return nil
}

func (c *CodexDesktopIPCClient) requestOnConn(conn net.Conn, method string, params map[string]any, timeout time.Duration, sourceClientID string, targetClientID string) (any, error) {
	rid := newUUID()
	if sourceClientID == "" {
		sourceClientID = c.clientID
	}
	frame := map[string]any{
		"type": "request", "requestId": rid, "sourceClientId": sourceClientID,
		"version": desktopRequestVersion(method), "method": method, "params": params,
	}
	if targetClientID != "" {
		frame["targetClientId"] = targetClientID
	}
	if err := writeDesktopFrame(conn, frame); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		msg, err := readDesktopFrame(conn)
		if err != nil {
			return nil, err
		}
		switch stringAny(msg["type"]) {
		case "client-discovery-request":
			_ = writeDesktopFrame(conn, map[string]any{
				"type":      "client-discovery-response",
				"requestId": stringAny(msg["requestId"]),
				"response":  map[string]any{"canHandle": false},
			})
			continue
		case "request":
			_ = writeDesktopFrame(conn, map[string]any{
				"type":       "response",
				"requestId":  stringAny(msg["requestId"]),
				"resultType": "error",
				"error":      "no-handler-for-request",
			})
			continue
		}
		if result, ok := desktopAsyncAccepted(method, params, msg); ok {
			return result, nil
		}
		if stringAny(msg["requestId"]) != rid && stringAny(msg["id"]) != rid {
			continue
		}
		if msg["type"] == "response" || msg["resultType"] != nil {
			if (msg["resultType"] == nil || msg["resultType"] == "success") && msg["error"] == nil {
				return msg["result"], nil
			}
			b, _ := json.Marshal(firstValue(msg["error"], msg))
			return nil, CodexDesktopIPCError{method + " failed: " + string(b)}
		}
		if msg["error"] != nil {
			b, _ := json.Marshal(msg["error"])
			return nil, CodexDesktopIPCError{method + " failed: " + string(b)}
		}
		if msg["result"] != nil {
			return msg["result"], nil
		}
	}
}

func writeDesktopFrame(w io.Writer, obj map[string]any) error {
	payload, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readDesktopFrame(r io.Reader) (map[string]any, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, desktopReadError(err)
	}
	size := binary.LittleEndian.Uint32(hdr[:])
	if size == 0 || size > 64*1024*1024 {
		return nil, CodexDesktopIPCError{"invalid Desktop IPC frame size: " + strconv.Itoa(int(size))}
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, desktopReadError(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, CodexDesktopIPCError{"invalid Desktop IPC JSON frame: " + err.Error()}
	}
	return msg, nil
}

func desktopReadError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return CodexDesktopIPCError{"Desktop IPC timeout waiting for frame"}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return CodexDesktopIPCError{"Desktop IPC socket closed"}
	}
	return CodexDesktopIPCError{"Desktop IPC read failed: " + err.Error()}
}

func inputText(prompt string) []map[string]any {
	return []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}
}

func desktopRequestVersion(method string) int {
	switch method {
	case "thread-follower-start-turn", "thread-follower-load-complete-history", "thread-follower-compact-thread", "thread-follower-steer-turn", "thread-follower-update-thread-settings",
		"thread-follower-command-approval-decision", "thread-follower-file-approval-decision",
		"thread-follower-permissions-request-approval-response", "thread-follower-submit-user-input",
		"thread-follower-submit-mcp-server-elicitation-response":
		return 1
	case "thread-follower-interrupt-turn":
		return 2
	default:
		return 0
	}
}

func defaultCodexDesktopSocket() string {
	uid := os.Getuid()
	name := "ipc-" + strconv.Itoa(uid) + ".sock"
	candidates := []string{}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		candidates = append(candidates, filepath.Join(tmp, "codex-ipc", name))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "codex-ipc", name), filepath.Join("/tmp", "codex-ipc", name))
	if matches, err := filepath.Glob(filepath.Join("/var/folders", "*", "*", "T", "codex-ipc", name)); err == nil {
		candidates = append(candidates, matches...)
	}
	candidates = dedupeStrings(candidates)
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aerr := os.Stat(candidates[i])
		bi, berr := os.Stat(candidates[j])
		if aerr == nil && berr == nil {
			return ai.ModTime().After(bi.ModTime())
		}
		return aerr == nil
	})
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func responseTurnID(result any) string {
	candidates := []any{result, mapAny(result)["result"]}
	if rr := mapAny(mapAny(result)["result"]); len(rr) > 0 {
		candidates = append(candidates, rr["result"])
	}
	for _, raw := range candidates {
		m := mapAny(raw)
		if id := firstNonEmpty(stringAny(m["turnId"]), stringAny(m["interruptedTurnId"])); id != "" {
			return id
		}
		if id := stringAny(mapAny(m["turn"])["id"]); id != "" {
			return id
		}
	}
	return ""
}

func desktopSnapshotLiveRows(msg map[string]any) []map[string]any {
	if msg["type"] != "broadcast" || stringAny(msg["method"]) != "thread-stream-state-changed" {
		return nil
	}
	params := mapAny(msg["params"])
	threadID := firstNonEmpty(stringAny(params["conversationId"]), stringAny(params["threadId"]))
	state := mapAny(mapAny(params["change"])["conversationState"])
	if threadID == "" {
		threadID = firstNonEmpty(stringAny(state["id"]), stringAny(state["sessionId"]))
	}
	if threadID == "" {
		return nil
	}
	status, updatedAt, live := desktopConversationLiveStatus(state)
	cwd := desktopConversationCwd(state)
	row := map[string]any{
		"session_id":        threadID,
		"provider_id":       "codex",
		"native_session_id": threadID,
		"transcript_id":     threadID,
		"codex_thread_id":   threadID,
		"title":             desktopConversationTitle(state, threadID),
		"cwd":               cwd,
		"live":              live,
		"state":             "idle",
		"status":            status,
		"updated_at":        updatedAt,
	}
	if live {
		row["state"] = "running"
	}
	if owner := stringAny(msg["sourceClientId"]); owner != "" {
		row["desktop_owner_client_id"] = owner
	}
	return []map[string]any{row}
}

func desktopAsyncAccepted(method string, params map[string]any, msg map[string]any) (any, bool) {
	if method != "thread-follower-start-turn" {
		return nil, false
	}
	want := firstNonEmpty(stringAny(params["conversationId"]), stringAny(params["threadId"]))
	if want == "" {
		return nil, false
	}
	for _, row := range desktopSnapshotLiveRows(msg) {
		tid := firstNonEmpty(stringAny(row["codex_thread_id"]), firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["native_session_id"])))
		if tid == want && boolAny(row["live"]) {
			return map[string]any{"accepted": true}, true
		}
	}
	return nil, false
}

func desktopConversationLiveStatus(state map[string]any) (string, string, bool) {
	status := ""
	updatedAt := ""
	live := false
	for _, turn := range mapsFromAny(state["turns"]) {
		st := stringAny(turn["status"])
		if st != "" {
			status = st
		}
		if ts := msToISO(firstValue(turn["updatedAtMs"], turn["turnStartedAtMs"])); ts != "" {
			updatedAt = ts
		}
		if desktopTurnStatusLive(st) {
			live = true
		}
	}
	return status, updatedAt, live
}

func desktopTurnStatusLive(status string) bool {
	switch stringsLower(status) {
	case "", "idle", "completed", "failed", "canceled", "cancelled", "interrupted":
		return false
	default:
		return true
	}
}

func desktopConversationCwd(state map[string]any) string {
	if cwd := stringAny(state["cwd"]); cwd != "" {
		return cwd
	}
	turns := mapsFromAny(state["turns"])
	for i := len(turns) - 1; i >= 0; i-- {
		if cwd := stringAny(mapAny(turns[i]["params"])["cwd"]); cwd != "" {
			return cwd
		}
	}
	return ""
}

func desktopConversationTitle(state map[string]any, threadID string) string {
	title := codexThreadTitle(state)
	if title != "" && title != "session "+shortText(threadID, 8) {
		return title
	}
	turns := mapsFromAny(state["turns"])
	for i := len(turns) - 1; i >= 0; i-- {
		params := mapAny(turns[i]["params"])
		if text := blocksText(params["input"], map[string]bool{"text": true}); text != "" {
			return shortText(text, 80)
		}
		for _, item := range mapsFromAny(turns[i]["items"]) {
			if stringAny(item["type"]) == "userMessage" {
				if text := blocksText(item["content"], map[string]bool{"text": true}); text != "" {
					return shortText(text, 80)
				}
			}
		}
	}
	return title
}
