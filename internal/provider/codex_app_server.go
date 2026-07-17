package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type CodexAppServerError struct {
	Message string
}

func (e CodexAppServerError) Error() string { return e.Message }

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

	cmd          *exec.Cmd
	stdin        io.WriteCloser
	nextID       atomic.Int64
	writeMu      sync.Mutex
	mu           sync.Mutex
	pending      map[int64]chan map[string]any
	threadStatus map[string]string
	threadTurn   map[string]string
	lastModel    string
	closed       bool
}

func NewCodexAppServerClient(command []string, cwd string, onNotification func(string, map[string]any), onServerRequest func(any, string, map[string]any)) *CodexAppServerClient {
	if len(command) == 0 {
		command = []string{"codex", "app-server"}
	}
	return &CodexAppServerClient{
		Command: command, Cwd: cwd, OnNotification: onNotification, OnServerRequest: onServerRequest,
		pending: map[int64]chan map[string]any{}, threadStatus: map[string]string{}, threadTurn: map[string]string{},
	}
}

func (c *CodexAppServerClient) Start() error {
	if c.cmd != nil {
		return nil
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	cmd.Dir = c.Cwd
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	c.stdin = stdin
	go c.readLoop(stdout)
	return nil
}

func (c *CodexAppServerClient) Close() error {
	c.mu.Lock()
	c.closed = true
	cmd := c.cmd
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func (c *CodexAppServerClient) Initialize(name string) error {
	_, err := c.request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": name, "title": name, "version": "0.0.1"},
		"capabilities": nil,
	}, 30*time.Second)
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
	res, err := c.request("thread/start", compactMap(params), 60*time.Second)
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
	return c.request("thread/fork", compactMap(params), 60*time.Second)
}

func (c *CodexAppServerClient) ThreadRollback(threadID string, numTurns int, params map[string]any) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["threadId"] = threadID
	params["numTurns"] = numTurns
	return c.request("thread/rollback", compactMap(params), 60*time.Second)
}

func (c *CodexAppServerClient) ThreadList(timeout time.Duration, params map[string]any) (any, error) {
	return c.request("thread/list", compactMap(params), timeout)
}

func (c *CodexAppServerClient) TurnStart(threadID string, prompt string, extra map[string]any) (any, error) {
	return c.TurnStartWithAttachments(threadID, prompt, nil, extra)
}

func (c *CodexAppServerClient) TurnStartWithAttachments(threadID string, prompt string, attachments []Attachment, extra map[string]any) (any, error) {
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
	res, err := c.request("turn/start", compactMap(params), 60*time.Second)
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
	res, err := c.request("turn/steer", compactMap(params), 60*time.Second)
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
	return c.request("turn/interrupt", compactMap(params), 60*time.Second)
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
	if c.stdin == nil {
		return CodexAppServerError{"client not started"}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (c *CodexAppServerClient) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
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
		if v != nil {
			out[k] = v
		}
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
