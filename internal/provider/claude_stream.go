package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const claudeStreamOutputLimit = 1024 * 1024

type claudeStreamBackend struct {
	mu           sync.RWMutex
	byLogical    map[string]*claudeStreamSession
	byTranscript map[string]*claudeStreamSession
	onEvent      func(claudeRuntimeSession, json.RawMessage)
	onTranscript func(string, string)
}

type claudeStreamSession struct {
	mu           sync.RWMutex
	writeMu      sync.Mutex
	pendingMu    sync.Mutex
	logicalID    string
	transcriptID string
	cwd          string
	pid          int
	started      time.Time
	updated      time.Time
	alive        bool
	running      bool
	sawTextDelta bool
	lastError    string
	output       string
	stdin        *io.PipeWriter
	cancel       context.CancelFunc
	pending      map[string]chan claudeStreamControlResult
}

type claudeStreamControlResult struct {
	response map[string]any
	err      error
}

type claudeStreamStart struct {
	LogicalID    string
	TranscriptID string
	Executable   string
	Args         []string
	Cwd          string
	Env          []string
}

func newClaudeStreamBackend(onEvent func(claudeRuntimeSession, json.RawMessage), onTranscript func(string, string)) *claudeStreamBackend {
	return &claudeStreamBackend{
		byLogical: map[string]*claudeStreamSession{}, byTranscript: map[string]*claudeStreamSession{},
		onEvent: onEvent, onTranscript: onTranscript,
	}
}

func (b *claudeStreamBackend) Start(opts claudeStreamStart) error {
	if opts.LogicalID == "" || opts.TranscriptID == "" || opts.Executable == "" {
		return errors.New("Claude stream session requires logical id, transcript id, and executable")
	}
	if b.HasSession(opts.LogicalID) {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	s := &claudeStreamSession{
		logicalID: opts.LogicalID, transcriptID: opts.TranscriptID, cwd: opts.Cwd,
		started: time.Now(), updated: time.Now(), alive: true, stdin: inWriter, cancel: cancel,
		pending: map[string]chan claudeStreamControlResult{},
	}
	b.mu.Lock()
	if existing := b.byLogical[opts.LogicalID]; existing != nil {
		existing.mu.RLock()
		alive := existing.alive
		existing.mu.RUnlock()
		if alive {
			b.mu.Unlock()
			cancel()
			_ = inReader.Close()
			_ = inWriter.Close()
			_ = outReader.Close()
			_ = outWriter.Close()
			return nil
		}
		delete(b.byLogical, opts.LogicalID)
		for id, candidate := range b.byTranscript {
			if candidate == existing {
				delete(b.byTranscript, id)
			}
		}
	}
	b.byLogical[opts.LogicalID] = s
	b.byTranscript[opts.TranscriptID] = s
	b.mu.Unlock()

	started := make(chan int, 1)
	done := make(chan error, 1)
	go b.readOutput(s, outReader)
	go func() {
		cmd := exec.CommandContext(ctx, opts.Executable, opts.Args...)
		cmd.Dir = opts.Cwd
		cmd.Env = opts.Env
		cmd.Stderr = io.Discard
		childIn, err := cmd.StdinPipe()
		if err == nil {
			var childOut io.ReadCloser
			childOut, err = cmd.StdoutPipe()
			if err == nil {
				err = cmd.Start()
				if err == nil {
					started <- cmd.Process.Pid
					go func() {
						_, _ = io.Copy(childIn, inReader)
						_ = childIn.Close()
					}()
					stdoutDone := make(chan struct{})
					go func() {
						defer close(stdoutDone)
						_, _ = io.Copy(outWriter, childOut)
					}()
					err = cmd.Wait()
					<-stdoutDone
				}
			}
		}
		if childIn != nil {
			_ = childIn.Close()
		}
		_ = outWriter.Close()
		_ = inReader.Close()
		done <- err
		b.processExited(s, err)
	}()

	select {
	case pid := <-started:
		s.mu.Lock()
		s.pid = pid
		s.mu.Unlock()
		if _, err := b.ControlRequest(opts.LogicalID, map[string]any{"subtype": "initialize", "hooks": nil}, 15*time.Second); err != nil {
			b.CloseSession(opts.LogicalID)
			return fmt.Errorf("initialize Claude stream-json process: %w", err)
		}
		return nil
	case err := <-done:
		if err == nil {
			err = errors.New("Claude CLI exited before stream initialization")
		}
		b.remove(s)
		return err
	case <-time.After(10 * time.Second):
		cancel()
		b.remove(s)
		return errors.New("timed out starting Claude stream-json process")
	}
}

func (b *claudeStreamBackend) readOutput(s *claudeStreamSession, r io.Reader) {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && json.Valid(trimmed) {
			b.handleEvent(s, append(json.RawMessage(nil), trimmed...))
		}
		if err != nil {
			return
		}
	}
}

func (b *claudeStreamBackend) handleEvent(s *claudeStreamSession, raw json.RawMessage) {
	var event map[string]any
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	if actual := claudeStreamSessionID(event); actual != "" {
		b.updateTranscript(s, actual)
	}
	if stringAny(event["type"]) == "control_response" {
		response := mapAny(event["response"])
		requestID := stringAny(response["request_id"])
		if requestID != "" {
			s.pendingMu.Lock()
			waiter := s.pending[requestID]
			delete(s.pending, requestID)
			s.pendingMu.Unlock()
			if waiter != nil {
				result := claudeStreamControlResult{response: mapAny(response["response"])}
				if stringAny(response["subtype"]) == "error" {
					result.err = errors.New(firstNonEmpty(stringAny(response["error"]), "Claude control request failed"))
				}
				waiter <- result
			}
		}
	}
	s.mu.Lock()
	s.updated = time.Now()
	s.running = claudeStreamRunningAfterEvent(s.running, event)
	appendClaudeStreamOutput(s, event)
	snapshot := claudeRuntimeSession{
		SessionID: s.transcriptID, PID: s.pid, Cwd: s.cwd, Connected: s.started,
		Updated: s.updated, Running: s.running,
	}
	s.mu.Unlock()
	if b.onEvent != nil {
		b.onEvent(snapshot, raw)
	}
}

func (b *claudeStreamBackend) updateTranscript(s *claudeStreamSession, transcriptID string) {
	b.mu.Lock()
	s.mu.Lock()
	old := s.transcriptID
	logicalID := s.logicalID
	if old == transcriptID {
		s.mu.Unlock()
		b.mu.Unlock()
		return
	}
	s.transcriptID = transcriptID
	s.mu.Unlock()
	if b.byTranscript[old] == s {
		delete(b.byTranscript, old)
	}
	b.byTranscript[transcriptID] = s
	b.mu.Unlock()
	if b.onTranscript != nil {
		b.onTranscript(logicalID, transcriptID)
	}
}

func (b *claudeStreamBackend) Send(id string, payload json.RawMessage) error {
	s := b.find(id)
	if s == nil {
		return fmt.Errorf("Claude stream session is not running: %s", id)
	}
	s.mu.RLock()
	alive := s.alive
	stdin := s.stdin
	s.mu.RUnlock()
	if !alive || stdin == nil {
		return fmt.Errorf("Claude stream session is closed: %s", id)
	}
	line := bytes.TrimSpace(payload)
	if len(line) == 0 || !json.Valid(line) {
		return errors.New("Claude stream input must be valid JSON")
	}
	s.writeMu.Lock()
	_, err := stdin.Write(append(append([]byte(nil), line...), '\n'))
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	var message map[string]any
	_ = json.Unmarshal(line, &message)
	s.mu.Lock()
	if stringAny(message["type"]) == "user" {
		s.running = true
	}
	s.updated = time.Now()
	s.mu.Unlock()
	return nil
}

func (b *claudeStreamBackend) ControlRequest(id string, request map[string]any, timeout time.Duration) (map[string]any, error) {
	s := b.find(id)
	if s == nil {
		return nil, fmt.Errorf("Claude stream session is not running: %s", id)
	}
	requestID := "remote-control-" + newUUID()
	waiter := make(chan claudeStreamControlResult, 1)
	s.pendingMu.Lock()
	s.pending[requestID] = waiter
	s.pendingMu.Unlock()
	payload, err := json.Marshal(map[string]any{
		"type": "control_request", "request_id": requestID, "request": request,
	})
	if err == nil {
		err = b.Send(id, payload)
	}
	if err != nil {
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
		return nil, err
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	select {
	case result := <-waiter:
		return result.response, result.err
	case <-time.After(timeout):
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
		return nil, fmt.Errorf("timed out waiting for Claude control response: %s", stringAny(request["subtype"]))
	}
}

func (b *claudeStreamBackend) CloseSession(id string) bool {
	s := b.find(id)
	if s == nil {
		return false
	}
	s.mu.Lock()
	cancel := s.cancel
	stdin := s.stdin
	s.alive = false
	s.running = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	b.remove(s)
	return true
}

func (b *claudeStreamBackend) Close() error {
	b.mu.RLock()
	rows := make([]*claudeStreamSession, 0, len(b.byLogical))
	for _, s := range b.byLogical {
		rows = append(rows, s)
	}
	b.mu.RUnlock()
	for _, s := range rows {
		b.CloseSession(s.logicalID)
	}
	return nil
}

func (b *claudeStreamBackend) HasSession(id string) bool {
	s := b.find(id)
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.alive
}

func (b *claudeStreamBackend) SessionRunning(id string) (bool, bool) {
	s := b.find(id)
	if s == nil {
		return false, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running, s.alive
}

func (b *claudeStreamBackend) Sessions() []claudeRuntimeSession {
	b.mu.RLock()
	rows := make([]*claudeStreamSession, 0, len(b.byLogical))
	for _, s := range b.byLogical {
		rows = append(rows, s)
	}
	b.mu.RUnlock()
	out := make([]claudeRuntimeSession, 0, len(rows))
	for _, s := range rows {
		s.mu.RLock()
		if s.alive {
			out = append(out, claudeRuntimeSession{
				SessionID: s.transcriptID, PID: s.pid, Cwd: s.cwd, Connected: s.started,
				Updated: s.updated, Running: s.running,
			})
		}
		s.mu.RUnlock()
	}
	return out
}

func (b *claudeStreamBackend) Output(id string) string {
	s := b.find(id)
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.output
}

func (b *claudeStreamBackend) LastError(id string) string {
	s := b.find(id)
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastError
}

func (b *claudeStreamBackend) Cwd(id string) string {
	s := b.find(id)
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cwd
}

func (b *claudeStreamBackend) find(id string) *claudeStreamSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if s := b.byLogical[id]; s != nil {
		return s
	}
	return b.byTranscript[id]
}

func (b *claudeStreamBackend) processExited(s *claudeStreamSession, err error) {
	s.mu.Lock()
	s.alive = false
	s.running = false
	s.updated = time.Now()
	if err != nil && !errors.Is(err, context.Canceled) {
		s.lastError = err.Error()
	}
	s.mu.Unlock()
	s.pendingMu.Lock()
	for requestID, waiter := range s.pending {
		delete(s.pending, requestID)
		waiter <- claudeStreamControlResult{err: errors.New("Claude stream-json process exited")}
	}
	s.pendingMu.Unlock()
}

func (b *claudeStreamBackend) remove(s *claudeStreamSession) {
	b.mu.Lock()
	if b.byLogical[s.logicalID] == s {
		delete(b.byLogical, s.logicalID)
	}
	for id, candidate := range b.byTranscript {
		if candidate == s {
			delete(b.byTranscript, id)
		}
	}
	b.mu.Unlock()
}

func claudeStreamSessionID(value any) string {
	switch v := value.(type) {
	case map[string]any:
		for _, key := range []string{"session_id", "sessionId"} {
			if s, ok := v[key].(string); ok && s != "" {
				return s
			}
		}
		for _, key := range []string{"data", "message", "result"} {
			if child, ok := v[key]; ok {
				if s := claudeStreamSessionID(child); s != "" {
					return s
				}
			}
		}
	case []any:
		for _, child := range v {
			if s := claudeStreamSessionID(child); s != "" {
				return s
			}
		}
	}
	return ""
}

func claudeStreamRunningAfterEvent(current bool, event map[string]any) bool {
	switch stringAny(event["type"]) {
	case "result":
		return false
	case "assistant", "stream_event", "control_request":
		return true
	case "user":
		return event["isReplay"] != true
	case "system":
		if stringAny(event["subtype"]) == "init" {
			return false
		}
	}
	return current
}

func appendClaudeStreamOutput(s *claudeStreamSession, event map[string]any) {
	typ := stringAny(event["type"])
	text := ""
	switch typ {
	case "stream_event":
		streamEvent := mapAny(event["event"])
		if stringAny(streamEvent["type"]) == "content_block_delta" {
			delta := mapAny(streamEvent["delta"])
			if stringAny(delta["type"]) == "text_delta" {
				text = stringAny(delta["text"])
				s.sawTextDelta = s.sawTextDelta || text != ""
			}
		}
	case "assistant":
		if !s.sawTextDelta {
			for _, raw := range listAny(mapAny(event["message"])["content"]) {
				block := mapAny(raw)
				if stringAny(block["type"]) == "text" {
					text += stringAny(block["text"])
				}
			}
		}
	case "result":
		s.sawTextDelta = false
	}
	if text == "" {
		return
	}
	s.output += text
	if len(s.output) > claudeStreamOutputLimit {
		s.output = s.output[len(s.output)-claudeStreamOutputLimit:]
	}
}

func claudeStreamEnv() []string {
	return os.Environ()
}
