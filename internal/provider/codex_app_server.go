package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CodexAppServerError struct {
	Message string
}

func (e CodexAppServerError) Error() string { return e.Message }

const (
	codexAppServerWebSocketDialTimeout    = 2 * time.Second
	codexAppServerInitializeTimeout       = 3 * time.Second
	codexAppServerStdioInitializeTimeout  = 8 * time.Second
	codexAppServerThreadStartTimeout      = 15 * time.Second
	codexAppServerStdioThreadStartTimeout = 20 * time.Second
	codexAppServerMutationTimeout         = 5 * time.Second
	codexAppServerStdioResumeTimeout      = 8 * time.Second
	codexAppServerBackgroundTimeout       = 30 * time.Second
	codexAppServerStdioBackgroundTimeout  = 60 * time.Second
)

type codexAppServerJSONTransport interface {
	WriteJSON([]byte) error
	ReadJSON() ([]byte, error)
	Close() error
}

type codexAppServerStdioTransport struct {
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	scanner *bufio.Scanner
}

func newCodexAppServerStdioTransport(stdin io.WriteCloser, stdout io.ReadCloser) *codexAppServerStdioTransport {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &codexAppServerStdioTransport{stdin: stdin, stdout: stdout, scanner: scanner}
}

func (t *codexAppServerStdioTransport) WriteJSON(payload []byte) error {
	payload = append(append([]byte(nil), payload...), '\n')
	for len(payload) > 0 {
		n, err := t.stdin.Write(payload)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func (t *codexAppServerStdioTransport) ReadJSON() ([]byte, error) {
	for t.scanner.Scan() {
		line := t.scanner.Bytes()
		if len(line) != 0 {
			return append([]byte(nil), line...), nil
		}
	}
	if err := t.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (t *codexAppServerStdioTransport) Close() error {
	// Closing stdin preserves the prior graceful child shutdown behavior. The
	// read side remains open until the child exits so cmd.Wait and scanner EOF
	// retain a single lifecycle owner in readLoop.
	return t.stdin.Close()
}

type codexAppClient interface {
	Start() error
	Initialize(name string) error
	Close() error
	AccountRateLimits(timeout time.Duration) (any, error)
	AccountRead(timeout time.Duration) (any, error)
	ThreadStart(params map[string]any) (string, error)
	ThreadResume(threadID string, params map[string]any, timeout time.Duration) (any, error)
	ThreadFork(threadID string, params map[string]any) (any, error)
	ThreadRollback(threadID string, numTurns int, params map[string]any) (any, error)
	ThreadList(timeout time.Duration, params map[string]any) (any, error)
	TurnStart(threadID string, prompt string, extra map[string]any) (any, error)
	TurnSteer(threadID string, prompt string, extra map[string]any) (any, error)
	TurnInterrupt(threadID string, extra map[string]any) (any, error)
	Respond(requestID any, result map[string]any) error
	RespondError(requestID any, code int, message string) error
	IsActive(threadID string) bool
	ThreadStatus(threadID string) (string, bool)
	SetThreadStatus(threadID string, status string)
	ThreadTurn(threadID string) (string, bool)
	SetThreadTurn(threadID string, turnID string)
	LastModel() string
}

type CodexAppServerClient struct {
	Command         []string
	Cwd             string
	OnNotification  func(method string, params map[string]any)
	OnServerRequest func(requestID any, method string, params map[string]any)
	OnExit          func(error)

	cmd           *exec.Cmd
	transport     codexAppServerJSONTransport
	shared        bool
	sharedDaemon  CodexSharedDaemonStatus
	dialWebSocket func(CodexSharedDaemonStatus, time.Duration) (codexAppServerJSONTransport, error)
	nextID        atomic.Int64
	writeMu       sync.Mutex
	mu            sync.Mutex
	pending       map[int64]chan map[string]any
	threadStatus  map[string]string
	threadTurn    map[string]string
	lastModel     string
	closed        bool
	exited        bool
	exitErr       error
	done          chan struct{}
	doneOnce      sync.Once
}

func NewCodexAppServerClient(command []string, cwd string, onNotification func(string, map[string]any), onServerRequest func(any, string, map[string]any)) *CodexAppServerClient {
	if len(command) == 0 {
		command = []string{"codex", "app-server"}
	}
	return &CodexAppServerClient{
		Command: command, Cwd: cwd, OnNotification: onNotification, OnServerRequest: onServerRequest,
		pending: map[int64]chan map[string]any{}, threadStatus: map[string]string{}, threadTurn: map[string]string{},
		done: make(chan struct{}),
	}
}

func NewCodexSharedAppServerClient(status CodexSharedDaemonStatus, cwd string, onNotification func(string, map[string]any), onServerRequest func(any, string, map[string]any)) *CodexAppServerClient {
	client := NewCodexAppServerClient(nil, cwd, onNotification, onServerRequest)
	client.shared = true
	client.sharedDaemon = status
	client.dialWebSocket = func(status CodexSharedDaemonStatus, timeout time.Duration) (codexAppServerJSONTransport, error) {
		return dialCodexAppServerWebSocketStatus(status, timeout)
	}
	return client
}

func (c *CodexAppServerClient) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return CodexAppServerError{"client closed"}
	}
	if c.transport != nil {
		return nil
	}
	if c.shared {
		if c.sharedDaemon.SocketPath == "" {
			return CodexAppServerError{"codex shared app-server socket path is empty"}
		}
		transport, err := c.dialWebSocket(c.sharedDaemon, codexAppServerWebSocketDialTimeout)
		if err != nil {
			return err
		}
		c.transport = transport
		go c.readLoop(transport, nil)
		return nil
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	cmd.Dir = c.Cwd
	cmd.Stderr = io.Discard
	configureCodexOwnedProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return err
	}
	transport := newCodexAppServerStdioTransport(stdin, stdout)
	c.cmd = cmd
	c.transport = transport
	go c.readLoop(transport, cmd)
	return nil
}

func (c *CodexAppServerClient) Close() error {
	c.mu.Lock()
	c.closed = true
	cmd := c.cmd
	transport := c.transport
	shared := c.shared
	c.mu.Unlock()
	var closeErr error
	if transport != nil {
		closeErr = transport.Close()
	}
	if shared {
		if transport == nil {
			c.finish(CodexAppServerError{"client closed"})
		} else {
			select {
			case <-c.done:
			case <-time.After(2 * time.Second):
				c.finish(CodexAppServerError{"client closed"})
			}
		}
		return closeErr
	}
	if cmd == nil || cmd.Process == nil {
		c.finish(CodexAppServerError{"client closed"})
		return closeErr
	}
	_ = signalCodexOwnedProcess(cmd.Process, false)
	select {
	case <-c.done:
		return closeErr
	case <-time.After(2 * time.Second):
	}
	if cmd != nil && cmd.Process != nil {
		_ = signalCodexOwnedProcess(cmd.Process, true)
	}
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
	return closeErr
}

func (c *CodexAppServerClient) SetExitHandler(handler func(error)) {
	c.mu.Lock()
	c.OnExit = handler
	exited, err := c.exited, c.exitErr
	c.mu.Unlock()
	if exited && handler != nil {
		go handler(err)
	}
}

func (c *CodexAppServerClient) Done() <-chan struct{} { return c.done }

func (c *CodexAppServerClient) ExitError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

func (c *CodexAppServerClient) Initialize(name string) error {
	timeout := codexAppServerInitializeTimeout
	if !c.shared {
		// Legacy stdio must also cover a cold child startup. Keep the larger
		// allowance bounded so the longest synchronous relay path remains
		// below its 30 second HTTP timeout.
		timeout = codexAppServerStdioInitializeTimeout
	}
	_, err := c.request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": name, "title": name, "version": agentHaloClientVersion()},
		"capabilities": map[string]any{"experimentalApi": true},
	}, timeout)
	if err != nil {
		return err
	}
	return c.notify("initialized", nil)
}

func (c *CodexAppServerClient) AccountRateLimits(timeout time.Duration) (any, error) {
	return c.request("account/rateLimits/read", nil, timeout)
}

func (c *CodexAppServerClient) AccountRead(timeout time.Duration) (any, error) {
	return c.request("account/read", nil, timeout)
}

func (c *CodexAppServerClient) ThreadStart(params map[string]any) (string, error) {
	timeout := codexAppServerThreadStartTimeout
	if !c.shared {
		timeout = codexAppServerStdioThreadStartTimeout
	}
	res, err := c.request("thread/start", compactMap(params), timeout)
	if err != nil {
		return "", err
	}
	thread := mapAny(mapAny(res)["thread"])
	if model := stringAny(thread["model"]); model != "" {
		c.mu.Lock()
		c.lastModel = model
		c.mu.Unlock()
	}
	tid := stringAny(thread["id"])
	if tid == "" {
		return "", CodexAppServerError{"thread/start returned no thread id"}
	}
	return tid, nil
}

func (c *CodexAppServerClient) ThreadResume(threadID string, params map[string]any, timeout time.Duration) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["threadId"] = threadID
	return c.request("thread/resume", compactMap(params), timeout)
}

func (c *CodexAppServerClient) ThreadFork(threadID string, params map[string]any) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["threadId"] = threadID
	return c.request("thread/fork", compactMap(params), codexAppServerMutationTimeout)
}

func (c *CodexAppServerClient) ThreadRollback(threadID string, numTurns int, params map[string]any) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["threadId"] = threadID
	params["numTurns"] = numTurns
	return c.request("thread/rollback", compactMap(params), codexAppServerMutationTimeout)
}

func (c *CodexAppServerClient) ThreadList(timeout time.Duration, params map[string]any) (any, error) {
	return c.request("thread/list", compactMap(params), timeout)
}

func (c *CodexAppServerClient) TurnStart(threadID string, prompt string, extra map[string]any) (any, error) {
	return c.TurnStartWithAttachments(threadID, prompt, nil, extra)
}

func (c *CodexAppServerClient) TurnStartWithAttachments(threadID string, prompt string, attachments []Attachment, extra map[string]any) (any, error) {
	timeout := codexAppServerBackgroundTimeout
	if !c.shared {
		timeout = codexAppServerStdioBackgroundTimeout
	}
	return c.TurnStartWithAttachmentsTimeout(threadID, prompt, attachments, extra, timeout)
}

func (c *CodexAppServerClient) TurnStartWithAttachmentsTimeout(threadID string, prompt string, attachments []Attachment, extra map[string]any, timeout time.Duration) (any, error) {
	if c.IsActive(threadID) {
		return nil, CodexAppServerError{"thread " + threadID + " has a live turn in progress"}
	}
	params := map[string]any{
		"threadId": threadID,
		"input":    codexUserInput(prompt, attachments),
	}
	for k, v := range extra {
		params[k] = v
	}
	res, err := c.request("turn/start", compactMap(params), timeout)
	if err == nil {
		c.rememberTurn(threadID, res)
	}
	return res, err
}

func (c *CodexAppServerClient) TurnSteer(threadID string, prompt string, extra map[string]any) (any, error) {
	params := map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}},
	}
	for k, v := range extra {
		params[k] = v
	}
	if params["expectedTurnId"] == nil {
		turnID, ok := c.ThreadTurn(threadID)
		if !ok {
			return nil, CodexAppServerError{"no active turn to steer (unknown turnId)"}
		}
		params["expectedTurnId"] = turnID
	}
	res, err := c.request("turn/steer", compactMap(params), codexAppServerMutationTimeout)
	if err == nil {
		c.rememberTurn(threadID, res)
	}
	return res, err
}

func (c *CodexAppServerClient) TurnInterrupt(threadID string, extra map[string]any) (any, error) {
	params := map[string]any{"threadId": threadID}
	for k, v := range extra {
		params[k] = v
	}
	if params["turnId"] == nil {
		turnID, ok := c.ThreadTurn(threadID)
		if !ok {
			return nil, CodexAppServerError{"no active turn to interrupt (unknown turnId)"}
		}
		params["turnId"] = turnID
	}
	return c.request("turn/interrupt", compactMap(params), codexAppServerMutationTimeout)
}

func (c *CodexAppServerClient) Respond(requestID any, result map[string]any) error {
	return c.write(map[string]any{"id": requestID, "result": result})
}

func (c *CodexAppServerClient) RespondError(requestID any, code int, message string) error {
	return c.write(map[string]any{"id": requestID, "error": map[string]any{"code": code, "message": message}})
}

func (c *CodexAppServerClient) request(method string, params map[string]any, timeout time.Duration) (any, error) {
	id := c.nextID.Add(1)
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, CodexAppServerError{"client closed"}
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": compactMap(params)}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg := <-ch:
		if errPayload, ok := msg["error"]; ok {
			b, _ := json.Marshal(errPayload)
			return nil, CodexAppServerError{method + " failed: " + string(b)}
		}
		return msg["result"], nil
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, CodexAppServerError{"timeout waiting for " + method}
	}
}

func (c *CodexAppServerClient) notify(method string, params map[string]any) error {
	obj := map[string]any{"method": method}
	if params != nil {
		obj["params"] = params
	}
	return c.write(obj)
}

func (c *CodexAppServerClient) write(obj map[string]any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	c.mu.Lock()
	transport, closed := c.transport, c.closed
	c.mu.Unlock()
	if transport == nil {
		c.writeMu.Unlock()
		return CodexAppServerError{"client not started"}
	}
	if closed {
		c.writeMu.Unlock()
		return CodexAppServerError{"client closed"}
	}
	err = transport.WriteJSON(b)
	if err != nil {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
	}
	c.writeMu.Unlock()
	if err != nil {
		writeErr := CodexAppServerError{"write to codex app-server failed: " + err.Error()}
		// A failed write means the stream can no longer prove message
		// boundaries or delivery. Retire the whole generation immediately;
		// otherwise the read loop may remain blocked forever on an idle UDS
		// while ensureClient keeps reusing a dead writer.
		_ = transport.Close()
		c.mu.Lock()
		cmd := c.cmd
		c.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = signalCodexOwnedProcess(cmd.Process, true)
		}
		c.finish(writeErr)
		return writeErr
	}
	return nil
}

func (c *CodexAppServerClient) readLoop(transport codexAppServerJSONTransport, cmd *exec.Cmd) {
	var readErr error
	for {
		payload, err := transport.ReadJSON()
		if err != nil {
			readErr = err
			break
		}
		var msg map[string]any
		if err := json.Unmarshal(payload, &msg); err != nil {
			readErr = CodexAppServerError{"invalid JSON from codex app-server: " + err.Error()}
			break
		}
		if msg["id"] != nil && (msg["result"] != nil || msg["error"] != nil) {
			if id, ok := numericID(msg["id"]); ok {
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- msg
				}
			}
			continue
		}
		method := stringAny(msg["method"])
		params := mapAny(msg["params"])
		if method == "" {
			continue
		}
		if msg["id"] != nil {
			if c.OnServerRequest != nil {
				c.OnServerRequest(msg["id"], method, params)
			}
			continue
		}
		c.track(method, params)
		if c.OnNotification != nil {
			c.OnNotification(method, params)
		}
	}
	_ = transport.Close()
	if cmd != nil {
		if readErr != nil && cmd.Process != nil {
			_ = signalCodexOwnedProcess(cmd.Process, true)
		}
		waitErr := cmd.Wait()
		if readErr == nil || errors.Is(readErr, io.EOF) {
			readErr = waitErr
		}
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		readErr = CodexAppServerError{"codex app-server exited"}
	}
	c.finish(readErr)
}

func (c *CodexAppServerClient) finish(err error) {
	if err == nil {
		err = CodexAppServerError{"codex app-server exited"}
	}
	c.doneOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.exited = true
		c.exitErr = err
		pending := c.pending
		c.pending = map[int64]chan map[string]any{}
		handler := c.OnExit
		c.mu.Unlock()

		msg := map[string]any{"error": map[string]any{"code": -32099, "message": err.Error()}}
		for _, waiter := range pending {
			select {
			case waiter <- msg:
			default:
			}
		}
		close(c.done)
		if handler != nil {
			handler(err)
		}
	})
}

func (c *CodexAppServerClient) track(method string, params map[string]any) {
	tid := stringAny(params["threadId"])
	if method == "thread/status/changed" {
		status := stringAny(mapAny(params["status"])["type"])
		if tid != "" && status != "" {
			c.SetThreadStatus(tid, status)
		}
	}
	turnID := firstNonEmpty(stringAny(params["turnId"]), stringAny(mapAny(params["turn"])["id"]))
	if tid != "" && turnID != "" {
		c.SetThreadTurn(tid, turnID)
	}
}

func (c *CodexAppServerClient) rememberTurn(threadID string, res any) {
	m := mapAny(res)
	turnID := firstNonEmpty(stringAny(m["turnId"]), stringAny(mapAny(m["turn"])["id"]))
	if turnID != "" {
		c.SetThreadTurn(threadID, turnID)
	}
}

func (c *CodexAppServerClient) IsActive(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threadStatus[threadID] == "active"
}

func (c *CodexAppServerClient) ThreadStatus(threadID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.threadStatus[threadID]
	return v, ok
}

func (c *CodexAppServerClient) SetThreadStatus(threadID string, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threadStatus[threadID] = status
}

func (c *CodexAppServerClient) ThreadTurn(threadID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.threadTurn[threadID]
	return v, ok
}

func (c *CodexAppServerClient) SetThreadTurn(threadID string, turnID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threadTurn[threadID] = turnID
}

func (c *CodexAppServerClient) LastModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastModel
}

func numericID(v any) (int64, bool) {
	n, ok := numberToInt64(v)
	return n, ok
}

func compactMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range in {
		if v == nil {
			continue
		}
		if text, ok := v.(string); ok && strings.TrimSpace(text) == "" {
			// Empty protocol overrides are not equivalent to omission. In
			// particular, model="" prevents the managed daemon from applying
			// the device's configured default and makes the turn fail.
			continue
		}
		out[k] = v
	}
	return out
}

func isThreadNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := stringsLower(err.Error())
	return stringsContains(s, "thread not found") || stringsContains(s, "thread_not_found") || stringsContains(s, "threadnotfound")
}

func stringsLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func stringsContains(s, substr string) bool {
	if substr == "" {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var errClientNotStarted = errors.New("client not started")
