package provider

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/psyche08/remote-agent/internal/config"
)

const (
	defaultPTYIdleTimeout = 1500 * time.Millisecond
	defaultPTYKillGrace   = 2 * time.Second
	defaultPTYOutputLimit = 256 * 1024
	defaultPTYHistory     = 200
)

// PTYProvider is an explicitly configured, capability-limited fallback for
// interactive command-line agents which do not expose a structured protocol.
// It never performs shell expansion: Command and Args are passed directly to
// exec.Command, and one process/PTY is owned by one logical session.
type PTYProvider struct {
	id            string
	cfg           config.ProviderConfig
	command       string
	args          []string
	cwd           string
	promptSuffix  string
	interruptSeq  string
	allowRawKeys  bool
	idleTimeout   time.Duration
	killGrace     time.Duration
	outputLimit   int
	historyLimit  int
	maxSessions   int
	rows          uint16
	cols          uint16
	readyPattern  *regexp.Regexp
	configError   string
	mu            sync.RWMutex
	sessions      map[string]*ptyProviderSession
	streamMu      sync.RWMutex
	streamPublish func(target string, frame map[string]any)
}

type ptyProviderSession struct {
	id          string
	cwd         string
	command     *exec.Cmd
	terminal    *os.File
	startedAt   time.Time
	updatedAt   time.Time
	state       string
	lastError   string
	output      []byte
	turnOutput  []byte
	messages    []map[string]any
	assistantAt int
	turn        uint64
	live        bool
	closing     bool
	idleTimer   *time.Timer
	done        chan struct{}
	sanitizer   terminalSanitizer
	mu          sync.Mutex
}

func NewPTYProvider(id string, cfg config.ProviderConfig) *PTYProvider {
	p := &PTYProvider{
		id:           id,
		cfg:          cfg,
		command:      expandUser(cfg.Command),
		args:         append([]string(nil), cfg.Args...),
		cwd:          expandUser(firstNonEmpty(cfg.Cwd, "~/Developer")),
		promptSuffix: stringExtra(cfg.Extra, "prompt_suffix", "\r"),
		interruptSeq: stringExtra(cfg.Extra, "interrupt_sequence", "\x03"),
		allowRawKeys: boolExtra(cfg.Extra, "allow_raw_keys", false),
		idleTimeout:  durationMillisExtra(cfg.Extra, "idle_timeout_ms", defaultPTYIdleTimeout),
		killGrace:    durationMillisExtra(cfg.Extra, "kill_grace_ms", defaultPTYKillGrace),
		outputLimit:  boundedIntExtra(cfg.Extra, "max_output_bytes", defaultPTYOutputLimit, 4*1024, 4*1024*1024),
		historyLimit: boundedIntExtra(cfg.Extra, "max_history_items", defaultPTYHistory, 10, 2000),
		maxSessions:  boundedIntExtra(cfg.Extra, "max_sessions", 32, 1, 256),
		rows:         uint16(boundedIntExtra(cfg.Extra, "rows", 40, 10, 500)),
		cols:         uint16(boundedIntExtra(cfg.Extra, "cols", 120, 20, 1000)),
		sessions:     map[string]*ptyProviderSession{},
	}
	if pattern := stringExtra(cfg.Extra, "ready_pattern", ""); pattern != "" {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			p.configError = "invalid ready_pattern: " + err.Error()
		} else {
			p.readyPattern = compiled
		}
	}
	if strings.TrimSpace(p.command) == "" {
		p.configError = "PTY provider command is required"
	}
	return p
}

func (p *PTYProvider) ID() string { return p.id }

func (p *PTYProvider) Installed() bool {
	return p.configError == "" && p.resolveCommand() != ""
}

func (p *PTYProvider) SetStreamPublisher(publish func(target string, frame map[string]any)) {
	p.streamMu.Lock()
	p.streamPublish = publish
	p.streamMu.Unlock()
}

func (p *PTYProvider) Status() Status {
	installed := p.Installed()
	state := "idle"
	var lastError string
	p.mu.RLock()
	sessions := make([]*ptyProviderSession, 0, len(p.sessions))
	for _, session := range p.sessions {
		sessions = append(sessions, session)
	}
	p.mu.RUnlock()
	for _, session := range sessions {
		session.mu.Lock()
		if session.state == "running" {
			state = "running"
		}
		if lastError == "" && session.lastError != "" {
			lastError = session.lastError
		}
		session.mu.Unlock()
	}
	if p.configError != "" {
		state = "error"
		lastError = p.configError
	} else if !installed {
		lastError = "configured command not found"
	}
	var errPtr *string
	if lastError != "" {
		errPtr = &lastError
	}
	return Status{
		ProviderID:  p.id,
		AppName:     firstNonEmpty(p.cfg.AppName, p.id),
		IsRunning:   installed,
		IsFrontmost: false,
		Installed:   installed,
		State:       state,
		LastError:   errPtr,
		Capabilities: map[string]bool{
			"native_sessions":        false,
			"native_task_status":     false,
			"approval":               false,
			"interrupt":              true,
			"steer":                  false,
			"streaming":              true,
			"create_session":         true,
			"attachments":            false,
			"raw_keys":               p.allowRawKeys,
			"pty":                    true,
			"best_effort_completion": true,
		},
		Backend: "generic_pty",
		Command: p.command,
		Cwd:     p.cwd,
	}
}

func (p *PTYProvider) ModelSelect() ModelSelect {
	return ModelSelect{Note: "generic PTY providers do not expose structured model or mode controls"}
}

func (p *PTYProvider) ListNativeSessions() []map[string]any {
	return p.RuntimeSessions()
}

func (p *PTYProvider) RuntimeSessions() []map[string]any {
	p.mu.RLock()
	sessions := make([]*ptyProviderSession, 0, len(p.sessions))
	for _, session := range p.sessions {
		sessions = append(sessions, session)
	}
	p.mu.RUnlock()
	rows := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		session.mu.Lock()
		row := map[string]any{
			"session_id":        session.id,
			"native_session_id": session.id,
			"transcript_id":     session.id,
			"provider_id":       p.id,
			"title":             firstNonEmpty(p.cfg.AppName, p.id),
			"cwd":               session.cwd,
			"source":            "generic_pty",
			"state":             session.state,
			"status":            session.state,
			"running":           session.state == "running",
			"live":              session.live,
			"created_at":        session.startedAt.UTC().Format(time.RFC3339Nano),
			"updated_at":        session.updatedAt.UTC().Format(time.RFC3339Nano),
		}
		session.mu.Unlock()
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return stringAny(rows[i]["updated_at"]) > stringAny(rows[j]["updated_at"])
	})
	return rows
}

func (p *PTYProvider) SessionMessages(sessionID string) ([]map[string]any, error) {
	session := p.session(sessionID)
	if session == nil {
		return nil, fmt.Errorf("PTY session is not running: %s", sessionID)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	out := make([]map[string]any, 0, len(session.messages))
	for _, item := range session.messages {
		copyItem := make(map[string]any, len(item))
		for key, value := range item {
			copyItem[key] = value
		}
		out = append(out, copyItem)
	}
	return out, nil
}

func (p *PTYProvider) SessionModel(string) map[string]any { return map[string]any{} }

func (p *PTYProvider) ReferencedFiles(string) map[string]bool { return map[string]bool{} }

func (p *PTYProvider) OpenOrCreateSession(sessionID string, opts StartOptions) (string, error) {
	if sessionID == "" {
		return "", errors.New("session_id is required")
	}
	if p.configError != "" {
		return "", errors.New(p.configError)
	}
	command := p.resolveCommand()
	if command == "" {
		return "", fmt.Errorf("PTY provider command not found: %s", p.command)
	}
	p.mu.Lock()
	if current := p.sessions[sessionID]; current != nil {
		current.mu.Lock()
		live := current.live
		current.mu.Unlock()
		p.mu.Unlock()
		if live {
			return sessionID, nil
		}
		return "", errors.New("PTY session process has exited; close it and create a new session")
	}
	p.pruneExitedSessionsLocked(p.maxSessions - 1)
	if len(p.sessions) >= p.maxSessions {
		p.mu.Unlock()
		return "", fmt.Errorf("PTY provider session limit reached (%d)", p.maxSessions)
	}
	cwd := expandUser(firstNonEmpty(opts.Cwd, p.cwd))
	if stat, err := os.Stat(cwd); err != nil || !stat.IsDir() {
		p.mu.Unlock()
		return "", fmt.Errorf("PTY cwd is not a directory: %s", cwd)
	}
	cmd := exec.Command(command, p.args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: p.rows, Cols: p.cols})
	if err != nil {
		p.mu.Unlock()
		return "", fmt.Errorf("start PTY provider: %w", err)
	}
	now := time.Now()
	session := &ptyProviderSession{
		id: sessionID, cwd: cwd, command: cmd, terminal: terminal,
		startedAt: now, updatedAt: now, state: "idle", live: true,
		assistantAt: -1, done: make(chan struct{}),
	}
	p.sessions[sessionID] = session
	p.mu.Unlock()
	go p.readSession(session)
	return sessionID, nil
}

func (p *PTYProvider) CloseSession(sessionID string) map[string]any {
	p.mu.Lock()
	session := p.sessions[sessionID]
	if session != nil {
		delete(p.sessions, sessionID)
	}
	p.mu.Unlock()
	if session == nil {
		return map[string]any{"ok": true, "killed": false}
	}
	killed := p.stopSession(session)
	return map[string]any{"ok": true, "killed": killed}
}

func (p *PTYProvider) SendPrompt(sessionID string, prompt string) SendResult {
	if strings.TrimSpace(prompt) == "" {
		message := "empty prompt"
		return SendResult{OK: false, State: "idle", Error: &message}
	}
	session := p.session(sessionID)
	if session == nil {
		message := "PTY session is not running"
		return SendResult{OK: false, State: "error", Error: &message}
	}
	now := time.Now()
	session.mu.Lock()
	if !session.live || session.terminal == nil {
		session.mu.Unlock()
		message := "PTY session process has exited"
		return SendResult{OK: false, State: "error", Error: &message}
	}
	if session.state == "running" {
		session.mu.Unlock()
		message := "PTY session already has a running turn"
		return SendResult{OK: false, State: "running", Error: &message}
	}
	session.turn++
	turn := session.turn
	session.state = "running"
	session.updatedAt = now
	session.lastError = ""
	session.turnOutput = nil
	session.messages = append(session.messages,
		map[string]any{"role": "user", "kind": "text", "text": prompt, "ts": now.UTC().Format(time.RFC3339Nano)},
		map[string]any{"role": "assistant", "kind": "text", "text": "", "ts": now.UTC().Format(time.RFC3339Nano)},
	)
	session.assistantAt = len(session.messages) - 1
	session.trimHistoryLocked(p.historyLimit)
	terminal := session.terminal
	session.mu.Unlock()

	p.publish(sessionID, map[string]any{"type": "turn", "status": "started", "turn_id": fmt.Sprintf("pty-%d", turn)})
	if _, err := io.WriteString(terminal, prompt+p.promptSuffix); err != nil {
		session.mu.Lock()
		session.state = "error"
		session.lastError = "write to PTY failed"
		session.updatedAt = time.Now()
		session.mu.Unlock()
		message := "write to PTY failed"
		return SendResult{OK: false, State: "error", Error: &message}
	}
	p.scheduleIdle(session, turn)
	return SendResult{
		OK: true, State: "running", Message: "prompt written to generic PTY",
		NativeTaskID: fmt.Sprintf("%s:pty-%d", sessionID, turn),
	}
}

func (p *PTYProvider) LatestOutput(sessionID string) map[string]any {
	session := p.session(sessionID)
	if session == nil {
		return map[string]any{"source": "generic_pty", "text": "", "approval_required": false}
	}
	session.mu.Lock()
	text := string(append([]byte(nil), session.output...))
	session.mu.Unlock()
	return map[string]any{"source": "generic_pty", "text": text, "approval_required": false}
}

func (p *PTYProvider) DetectState(sessionID string) string {
	session := p.session(sessionID)
	if session == nil {
		return "idle"
	}
	session.mu.Lock()
	state := session.state
	session.mu.Unlock()
	return state
}

func (p *PTYProvider) SessionRunning(sessionID string) *bool {
	session := p.session(sessionID)
	if session == nil {
		return nil
	}
	session.mu.Lock()
	running := session.live && session.state == "running"
	session.mu.Unlock()
	return &running
}

func (p *PTYProvider) RelayApproval(string, string) map[string]any {
	return map[string]any{"ok": false, "detail": "generic PTY providers do not expose structured approvals"}
}

func (p *PTYProvider) SendKeys(sessionID string, keys []string) map[string]any {
	if !p.allowRawKeys {
		return map[string]any{"ok": false, "detail": "raw keys are disabled for this PTY provider"}
	}
	payload, err := encodePTYKeys(keys)
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	session := p.session(sessionID)
	if session == nil {
		return map[string]any{"ok": false, "detail": "PTY session is not running"}
	}
	session.mu.Lock()
	if !session.live || session.terminal == nil {
		session.mu.Unlock()
		return map[string]any{"ok": false, "detail": "PTY session process has exited"}
	}
	terminal := session.terminal
	session.mu.Unlock()
	if _, err := terminal.Write(payload); err != nil {
		return map[string]any{"ok": false, "detail": "write to PTY failed"}
	}
	return map[string]any{"ok": true}
}

func (p *PTYProvider) Interrupt(sessionID string) map[string]any {
	session := p.session(sessionID)
	if session == nil {
		return map[string]any{"ok": false, "detail": "PTY session is not running"}
	}
	session.mu.Lock()
	if !session.live || session.terminal == nil {
		session.mu.Unlock()
		return map[string]any{"ok": false, "detail": "PTY session process has exited"}
	}
	terminal := session.terminal
	session.state = "idle"
	session.updatedAt = time.Now()
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	session.mu.Unlock()
	if _, err := io.WriteString(terminal, p.interruptSeq); err != nil {
		return map[string]any{"ok": false, "detail": "write interrupt to PTY failed"}
	}
	p.publish(sessionID, map[string]any{"type": "turn", "status": "completed", "turn_id": nil})
	return map[string]any{"ok": true, "detail": "configured PTY interrupt sequence sent"}
}

func (p *PTYProvider) SetSessionModel(string, string, string) map[string]any {
	return map[string]any{"ok": false, "detail": "generic PTY providers do not expose structured model controls"}
}

func (p *PTYProvider) Shutdown() {
	p.mu.Lock()
	sessions := make([]*ptyProviderSession, 0, len(p.sessions))
	for id, session := range p.sessions {
		sessions = append(sessions, session)
		delete(p.sessions, id)
	}
	p.mu.Unlock()
	for _, session := range sessions {
		p.stopSession(session)
	}
}

func (p *PTYProvider) session(sessionID string) *ptyProviderSession {
	p.mu.RLock()
	session := p.sessions[sessionID]
	p.mu.RUnlock()
	return session
}

func (p *PTYProvider) resolveCommand() string {
	if filepath.IsAbs(p.command) {
		info, err := os.Stat(p.command)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p.command
		}
		return ""
	}
	path, err := exec.LookPath(p.command)
	if err != nil {
		return ""
	}
	return path
}

func (p *PTYProvider) readSession(session *ptyProviderSession) {
	defer close(session.done)
	terminal := session.terminal
	buffer := make([]byte, 8192)
	for {
		n, err := terminal.Read(buffer)
		if n > 0 {
			if text := session.sanitizer.Write(buffer[:n]); text != "" {
				p.handleOutput(session, text)
			}
		}
		if err != nil {
			break
		}
	}
	_ = terminal.Close()
	waitErr := session.command.Wait()
	session.mu.Lock()
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	wasClosing := session.closing
	if session.terminal == terminal {
		session.terminal = nil
	}
	session.live = false
	session.updatedAt = time.Now()
	if wasClosing {
		session.state = "idle"
	} else if waitErr != nil {
		session.state = "error"
		session.lastError = "PTY process exited unexpectedly"
	} else {
		session.state = "idle"
	}
	session.mu.Unlock()
	p.publish(session.id, map[string]any{"type": "turn", "status": "completed", "turn_id": nil})
}

// pruneExitedSessionsLocked bounds retained preview/history state while
// preserving active children. p.mu must be held by the caller.
func (p *PTYProvider) pruneExitedSessionsLocked(keep int) {
	if len(p.sessions) <= keep {
		return
	}
	type candidate struct {
		id        string
		updatedAt time.Time
	}
	exited := make([]candidate, 0, len(p.sessions))
	for id, session := range p.sessions {
		session.mu.Lock()
		live, updatedAt := session.live, session.updatedAt
		session.mu.Unlock()
		if !live {
			exited = append(exited, candidate{id: id, updatedAt: updatedAt})
		}
	}
	sort.Slice(exited, func(i, j int) bool { return exited[i].updatedAt.Before(exited[j].updatedAt) })
	for _, item := range exited {
		if len(p.sessions) <= keep {
			return
		}
		delete(p.sessions, item.id)
	}
}

func (p *PTYProvider) handleOutput(session *ptyProviderSession, text string) {
	now := time.Now()
	session.mu.Lock()
	session.updatedAt = now
	session.output = appendTail(session.output, []byte(text), p.outputLimit)
	session.turnOutput = appendTail(session.turnOutput, []byte(text), p.outputLimit)
	if session.assistantAt >= 0 && session.assistantAt < len(session.messages) {
		item := session.messages[session.assistantAt]
		item["text"] = appendTextTail(stringAny(item["text"]), text, p.outputLimit)
		item["ts"] = now.UTC().Format(time.RFC3339Nano)
	} else {
		session.messages = append(session.messages, map[string]any{
			"role": "assistant", "kind": "text", "text": appendTextTail("", text, p.outputLimit),
			"ts": now.UTC().Format(time.RFC3339Nano),
		})
		session.assistantAt = len(session.messages) - 1
		session.trimHistoryLocked(p.historyLimit)
	}
	running := session.state == "running"
	turn := session.turn
	ready := running && p.readyPattern != nil && p.readyPattern.Match(session.turnOutput)
	session.mu.Unlock()
	p.publish(session.id, map[string]any{"type": "delta", "turn_id": fmt.Sprintf("pty-%d", turn), "text": text})
	if ready {
		p.completeTurn(session, turn)
	} else if running {
		p.scheduleIdle(session, turn)
	}
}

func (p *PTYProvider) scheduleIdle(session *ptyProviderSession, turn uint64) {
	session.mu.Lock()
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	session.idleTimer = time.AfterFunc(p.idleTimeout, func() {
		p.completeTurn(session, turn)
	})
	session.mu.Unlock()
}

func (p *PTYProvider) completeTurn(session *ptyProviderSession, turn uint64) {
	session.mu.Lock()
	if !session.live || session.turn != turn || session.state != "running" {
		session.mu.Unlock()
		return
	}
	session.state = "idle"
	session.updatedAt = time.Now()
	session.mu.Unlock()
	p.publish(session.id, map[string]any{
		"type": "turn", "status": "completed", "turn_id": fmt.Sprintf("pty-%d", turn),
	})
}

func (p *PTYProvider) stopSession(session *ptyProviderSession) bool {
	session.mu.Lock()
	if session.closing {
		done := session.done
		session.mu.Unlock()
		select {
		case <-done:
		case <-time.After(p.killGrace):
		}
		return true
	}
	session.closing = true
	if session.idleTimer != nil {
		session.idleTimer.Stop()
	}
	process := session.command.Process
	terminal := session.terminal
	done := session.done
	session.mu.Unlock()
	if process == nil {
		return false
	}
	_ = syscall.Kill(-process.Pid, syscall.SIGTERM)
	_ = process.Signal(syscall.SIGTERM)
	if terminal != nil {
		_ = terminal.Close()
	}
	select {
	case <-done:
	case <-time.After(p.killGrace):
		_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
		_ = process.Kill()
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
		}
	}
	return true
}

func (p *PTYProvider) publish(target string, frame map[string]any) {
	p.streamMu.RLock()
	publish := p.streamPublish
	p.streamMu.RUnlock()
	if publish != nil {
		publish(target, frame)
	}
}

func (s *ptyProviderSession) trimHistoryLocked(limit int) {
	if len(s.messages) <= limit {
		return
	}
	drop := len(s.messages) - limit
	s.messages = append([]map[string]any(nil), s.messages[drop:]...)
	if s.assistantAt >= 0 {
		s.assistantAt -= drop
		if s.assistantAt < 0 {
			s.assistantAt = -1
		}
	}
}

func appendTail(current []byte, incoming []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if len(incoming) >= limit {
		return append([]byte(nil), incoming[len(incoming)-limit:]...)
	}
	if len(current)+len(incoming) > limit {
		current = append([]byte(nil), current[len(current)+len(incoming)-limit:]...)
	}
	return append(current, incoming...)
}

func appendTextTail(current string, incoming string, limit int) string {
	return string(appendTail([]byte(current), []byte(incoming), limit))
}

func durationMillisExtra(values map[string]any, key string, fallback time.Duration) time.Duration {
	switch value := values[key].(type) {
	case float64:
		if value > 0 {
			return time.Duration(value * float64(time.Millisecond))
		}
	case int:
		if value > 0 {
			return time.Duration(value) * time.Millisecond
		}
	}
	return fallback
}

func boundedIntExtra(values map[string]any, key string, fallback int, minimum int, maximum int) int {
	value := intExtra(values, key, fallback)
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func encodePTYKeys(keys []string) ([]byte, error) {
	var out []byte
	named := map[string]string{
		"ESC": "\x1b", "ENTER": "\r", "RETURN": "\r", "TAB": "\t",
		"BACKSPACE": "\x7f", "CTRL_C": "\x03", "CTRL_D": "\x04",
		"UP": "\x1b[A", "DOWN": "\x1b[B", "RIGHT": "\x1b[C", "LEFT": "\x1b[D",
	}
	for _, key := range keys {
		if value, ok := named[strings.ToUpper(key)]; ok {
			out = append(out, value...)
			continue
		}
		if utf8.RuneCountInString(key) == 1 {
			out = append(out, key...)
			continue
		}
		return nil, fmt.Errorf("unsupported PTY key: %q", key)
	}
	if len(out) == 0 {
		return nil, errors.New("keys are empty")
	}
	return out, nil
}

// terminalSanitizer strips terminal control protocols, including CSI and OSC,
// while retaining printable UTF-8 and ordinary line breaks for the web UI.
// Parser state and an incomplete UTF-8 suffix are preserved across reads.
type terminalSanitizer struct {
	state   byte
	pending []byte
}

func (s *terminalSanitizer) Write(data []byte) string {
	printable := make([]byte, 0, len(data))
	for _, value := range data {
		switch s.state {
		case 0:
			switch value {
			case 0x1b:
				s.state = 1
			case '\n', '\r', '\t':
				printable = append(printable, value)
			default:
				if value >= 0x20 && value != 0x7f {
					printable = append(printable, value)
				}
			}
		case 1:
			switch value {
			case '[':
				s.state = 2
			case ']', 'P', '_', '^', 'X':
				s.state = 3
			case '(', ')', '*', '+', '-', '.', '/':
				s.state = 5
			default:
				s.state = 0
			}
		case 2:
			if value >= 0x40 && value <= 0x7e {
				s.state = 0
			}
		case 3:
			if value == 0x07 {
				s.state = 0
			} else if value == 0x1b {
				s.state = 4
			}
		case 4:
			if value == '\\' {
				s.state = 0
			} else if value != 0x1b {
				s.state = 3
			}
		case 5:
			s.state = 0
		}
	}
	printable = append(s.pending, printable...)
	s.pending = nil
	if len(printable) > 0 {
		start := len(printable) - 1
		for start > 0 && len(printable)-start < utf8.UTFMax && printable[start]&0xc0 == 0x80 {
			start--
		}
		if printable[start] >= utf8.RuneSelf && !utf8.FullRune(printable[start:]) {
			s.pending = append([]byte(nil), printable[start:]...)
			printable = printable[:start]
		}
	}
	text := strings.ToValidUTF8(string(printable), "�")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}
