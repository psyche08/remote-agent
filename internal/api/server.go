package api

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psyche08/remote-agent/internal/buildinfo"
	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/pricing"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
	webui "github.com/psyche08/remote-agent/static"
)

type Server struct {
	cfg             *config.Config
	registry        provider.Registry
	store           *state.Store
	activeProvider  string
	activeSessionID *string
	mu              sync.Mutex
	resumeMu        sync.Mutex
	resumeInFlight  map[string]bool
	sendMu          sync.Mutex
	sendInFlight    map[string]bool
	streamMu        sync.Mutex
	streamSubs      map[string]map[chan []byte]bool
	presenceMu      sync.Mutex
	presence        map[string]time.Time
	pushMu          sync.Mutex
	pushLast        map[string]string
	pushStop        chan struct{}
	pushOnce        sync.Once
	pushSender      func(map[string]any) int
	updateMu        sync.Mutex
	updateRunning   bool
	nativeMu        sync.Mutex
	nativeCache     map[string]*nativeSessionCacheEntry
	clientMu        sync.Mutex
	clients         map[string]*clientVersionSeen
	pricing         *pricing.Manager
	lastScreenshot  string
	lastShotAt      string
}

type nativeSessionCacheEntry struct {
	sessions    []map[string]any
	refreshedAt time.Time
	refreshing  bool
	done        chan struct{}
	err         string
}

type nativeSessionMeta struct {
	Source      string
	RefreshedAt string
	Refreshing  bool
	Error       string
}

const (
	nativeSessionRefreshMin = 15 * time.Second
	nativeSessionBriefWait  = 150 * time.Millisecond
)

func NewServer(cfg *config.Config, registry provider.Registry, store *state.Store) *Server {
	active := canonicalProviderID(cfg.DefaultProvider)
	if _, ok := registry[active]; !ok {
		for _, id := range registry.IDs() {
			active = id
			break
		}
	}
	s := &Server{
		cfg: cfg, registry: registry, store: store, activeProvider: active,
		resumeInFlight: map[string]bool{}, sendInFlight: map[string]bool{},
		streamSubs: map[string]map[chan []byte]bool{}, presence: map[string]time.Time{},
		pushLast: map[string]string{}, pushStop: make(chan struct{}), nativeCache: map[string]*nativeSessionCacheEntry{},
		clients: map[string]*clientVersionSeen{}, pricing: pricing.New(store.DataDir()),
	}
	s.pushSender = func(payload map[string]any) int { return s.sendPushToAll(payload, true) }
	for _, id := range registry.IDs() {
		providerID := id
		if p, ok := registry[id].(interface {
			SetStreamPublisher(func(target string, frame map[string]any))
		}); ok {
			p.SetStreamPublisher(func(target string, frame map[string]any) {
				s.publishProviderStream(providerID, target, frame)
			})
		}
	}
	return s
}

func (s *Server) StartBackground() {
	s.StartBackgroundWithAutoUpdate(true)
}

func (s *Server) StartBackgroundWithAutoUpdate(autoUpdate bool) {
	s.pushOnce.Do(func() {
		go s.pushMonitorLoop()
		if autoUpdate {
			go s.autoUpdateLoop()
		}
		go s.watchdogLoop()
		s.pricing.Start(s.pushStop)
	})
}

func (s *Server) StopBackground() {
	select {
	case <-s.pushStop:
	default:
		close(s.pushStop)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/status", s.status)
	mux.HandleFunc("/providers", s.providers)
	mux.HandleFunc("/provider/select", s.providerSelect)
	mux.HandleFunc("/send_prompt", s.sendPrompt)
	mux.HandleFunc("/upload", s.upload)
	mux.HandleFunc("/session_asset", s.sessionAsset)
	mux.HandleFunc("/rewind_user_message", s.rewindUserMessage)
	mux.HandleFunc("/output", s.output)
	mux.HandleFunc("/screenshot", s.screenshot)
	mux.HandleFunc("/last_screenshot", s.lastScreenshotFile)
	mux.HandleFunc("/clipboard", s.clipboard)
	mux.HandleFunc("/copy_reply", s.copyReply)
	mux.HandleFunc("/recover", s.recover)
	mux.HandleFunc("/ocr", s.ocr)
	mux.HandleFunc("/sessions", s.sessions)
	mux.HandleFunc("/native_sessions", s.nativeSessions)
	mux.HandleFunc("/session_options", s.sessionOptions)
	mux.HandleFunc("/browse_dirs", s.browseDirs)
	mux.HandleFunc("/session_preview", s.sessionPreview)
	mux.HandleFunc("/file", s.file)
	mux.HandleFunc("/project_tree", s.projectTree)
	mux.HandleFunc("/project_file", s.projectFile)
	mux.HandleFunc("/git_log", s.gitLog)
	mux.HandleFunc("/git_commit", s.gitCommit)
	mux.HandleFunc("/resume_native_session", s.resumeNativeSession)
	mux.HandleFunc("/live_sessions", s.liveSessions)
	mux.HandleFunc("/close_session", s.closeSession)
	mux.HandleFunc("/tasks", s.tasks)
	mux.HandleFunc("/interrupt", s.interrupt)
	mux.HandleFunc("/keys", s.keys)
	mux.HandleFunc("/set_model", s.setModel)
	mux.HandleFunc("/steer", s.steer)
	mux.HandleFunc("/approval", s.approval)
	mux.HandleFunc("/question_answer", s.questionAnswer)
	mux.HandleFunc("/pending_approvals", s.pendingApprovals)
	mux.HandleFunc("/stream", s.stream)
	mux.HandleFunc("/push/vapid", s.pushVAPID)
	mux.HandleFunc("/push/subscribe", s.pushSubscribe)
	mux.HandleFunc("/push/unsubscribe", s.pushUnsubscribe)
	mux.HandleFunc("/push/presence", s.pushPresence)
	mux.HandleFunc("/push/approve", s.pushApprove)
	mux.HandleFunc("/push/test", s.pushTest)
	mux.HandleFunc("/update", s.update)
	mux.HandleFunc("/client_versions", s.clientVersions)
	mux.HandleFunc("/pricing", s.pricingStatus)
	// The full console is device-owned and embedded in the agent binary. The
	// relay root serves only a stable device host which frames this handler
	// through /s/remotecoding/d/<device>/ without leaving the root PWA URL.
	mux.Handle("/", webui.Handler(buildinfo.Commit))
	return s.captureClientVersion(mux)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": buildinfo.Info()})
}

func (s *Server) pricingStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.pricing.Status())
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	activeProvider := s.activeProvider
	activeSessionID := s.activeSessionID
	s.mu.Unlock()

	providerID := r.URL.Query().Get("provider_id")
	if providerID == "" {
		providerID = activeProvider
	}
	p, providerID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id: "+providerID)
		return
	}
	sidView := r.URL.Query().Get("session_id")
	if sidView == "" && providerID == activeProvider && activeSessionID != nil {
		sidView = *activeSessionID
	}
	if sidView != "" {
		if err := rejectUnsafeSessionID(sidView); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	s.bindProviderTranscript(p, sidView)
	stateValue := p.DetectState(sidView)
	if scoped, ok := p.(interface{ SessionRunning(string) *bool }); ok && sidView != "" {
		if running := scoped.SessionRunning(sidView); running != nil {
			if *running && stateValue != "waiting_approval" && stateValue != "waiting_input" {
				stateValue = "running"
			} else if stateValue == "running" {
				stateValue = "idle"
			}
		}
	}
	if sidView != "" && s.isSendInFlight(providerID, sidView) {
		stateValue = "delivering"
	}
	var approvalRequest map[string]any
	// Query the request itself even when DetectState raced an IPC update. A
	// pending request is authoritative and must force waiting_approval; gating
	// this lookup on the earlier state value could hide a request forever until
	// another provider event happened to change state.
	if ar, ok := p.(interface{ ApprovalRequest(string) map[string]any }); ok {
		approvalRequest = ar.ApprovalRequest(sidView)
		if approvalRequest != nil {
			stateValue = pendingInteractionState(approvalRequest)
		}
	}
	ps := p.Status()
	sessionLastError := ""
	if sidView != "" {
		if session, ok, err := s.findSessionForProviderAny(providerID, sidView); err == nil && ok {
			sessionLastError = recordString(session, "last_error")
			if approvalRequest == nil && recordString(session, "state") == "error" && sessionLastError != "" && !s.isSendInFlight(providerID, sidView) {
				stateValue = "error"
			}
		}
	}
	lastError := any(nil)
	if sessionLastError != "" {
		lastError = sessionLastError
	} else if ps.LastError != nil && *ps.LastError != "" {
		lastError = *ps.LastError
	}
	modelSelect := p.ModelSelect()
	resp := map[string]any{
		"device_id":               s.cfg.DeviceID,
		"devices":                 s.cfg.Devices,
		"agent_available":         true,
		"active_provider":         providerID,
		"active_session_id":       nullableActiveSession(activeProvider, providerID, activeSessionID),
		"provider_status":         ps,
		"state":                   stateValue,
		"last_prompt_at":          nil,
		"last_screenshot_at":      nil,
		"last_clipboard_at":       nil,
		"last_error":              lastError,
		"active_provider_running": ps.IsRunning,
		"version":                 buildinfo.Info(),
	}
	// Show the viewed session's real settings (mode/model/effort as owned by
	// the native runtime) instead of provider-global defaults.
	if sidView != "" {
		if sp, ok := p.(interface{ SessionSettings(string) map[string]any }); ok {
			if st := sp.SessionSettings(sidView); len(st) > 0 {
				resp["session_settings"] = st
				if mode := stringAny(st["mode"]); mode != "" {
					modelSelect.Mode = mode
				}
				if model := stringAny(st["model"]); model != "" {
					modelSelect.CurrentModel = &model
				}
				if effort := stringAny(st["effort"]); effort != "" {
					modelSelect.CurrentEffort = &effort
				}
			}
		}
	}
	resp["model_select"] = modelSelect
	if approvalRequest != nil {
		resp["approval_request"] = approvalRequest
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	// Providers whose app/CLI is not installed on this device are hidden so
	// the web console never offers them. Re-checked per request, so
	// installing the CLI later surfaces the provider without a restart.
	// ?include_uninstalled=1 keeps them visible for debugging.
	includeUninstalled := r.URL.Query().Get("include_uninstalled") == "1" ||
		r.URL.Query().Get("include_uninstalled") == "true"
	rows := []map[string]any{}
	visible := map[string]bool{}
	for _, id := range s.registry.IDs() {
		p := s.registry[id]
		if !includeUninstalled {
			if checker, ok := p.(provider.InstallChecker); ok && !checker.Installed() {
				continue
			}
		}
		visible[id] = true
		st := p.Status()
		rows = append(rows, map[string]any{
			"provider_id":  id,
			"status":       st,
			"capabilities": st.Capabilities,
			"model_select": p.ModelSelect(),
		})
	}
	s.mu.Lock()
	active := s.activeProvider
	s.mu.Unlock()
	if !visible[active] && len(rows) > 0 {
		active = stringAny(rows[0]["provider_id"])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active_provider": active,
		"providers":       rows,
	})
}

type providerSelectIn struct {
	ProviderID string `json:"provider_id"`
}

func (s *Server) providerSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body providerSelectIn
	if !decodeJSON(w, r, &body) {
		return
	}
	if _, id, ok := s.getProvider(body.ProviderID); !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id: "+body.ProviderID)
		return
	} else {
		s.mu.Lock()
		s.activeProvider = id
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active_provider": id})
	}
}

type createSessionIn struct {
	ProviderID string `json:"provider_id"`
	Title      string `json:"title"`
	Cwd        string `json:"cwd"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	Mode       string `json:"mode"`
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		records, err := s.store.Sessions()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": records})
	case http.MethodPost:
		var body createSessionIn
		if !decodeJSON(w, r, &body) {
			return
		}
		p, providerID, ok := s.getProvider(body.ProviderID)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown provider_id: "+body.ProviderID)
			return
		}
		opts, err := validateStartOptions(p, body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		session := newSessionRecord(s.cfg.DeviceID, providerID, body.Title, opts)
		native, err := p.OpenOrCreateSession(recordString(session, "session_id"), opts)
		if err != nil {
			session["last_error"] = err.Error()
		}
		if native != "" {
			session["native_session_id"] = native
			session["transcript_id"] = native
		}
		if err := s.store.UpsertSession(session); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.mu.Lock()
		s.activeProvider = providerID
		s.activeSessionID = stringPtr(recordString(session, "session_id"))
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type sendPromptIn struct {
	ProviderID  string   `json:"provider_id"`
	SessionID   string   `json:"session_id"`
	Prompt      string   `json:"prompt"`
	Attachments []string `json:"attachments"`
}

func (s *Server) sendPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body sendPromptIn
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" && len(body.Attachments) == 0 {
		writeError(w, http.StatusBadRequest, "prompt is empty")
		return
	}
	var session state.Record
	var p provider.Provider
	var providerID string
	var ok bool
	var err error
	if body.ProviderID != "" {
		if resolvedProvider, resolvedProviderID, providerOK := s.getProvider(body.ProviderID); providerOK {
			p, providerID = resolvedProvider, resolvedProviderID
			session, ok, err = s.findSessionForProviderAny(providerID, body.SessionID)
		} else {
			writeError(w, http.StatusBadRequest, "unknown provider_id: "+body.ProviderID)
			return
		}
	} else {
		session, ok, err = s.findSessionAny(body.SessionID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A native Codex preview can resolve to an existing logical session by its
	// transcript id. It still represents an explicit Desktop-native delivery
	// request, so route it through prepareDirectCodexSession even though the
	// record lookup succeeded. Requests using the logical session id retain the
	// record's existing app-server/Desktop route.
	directCodexPreview := providerID == "codex" && (!ok || (recordString(session, "session_id") != "" && body.SessionID != recordString(session, "session_id")))
	if directCodexPreview {
		session, err = s.prepareDirectCodexSession(p, providerID, body.SessionID)
		if err == nil && session != nil {
			ok = true
			body.SessionID = recordString(session, "session_id")
		}
	}
	if !ok {
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeError(w, http.StatusNotFound, "unknown session_id: "+body.SessionID)
		return
	}
	if logicalID := recordString(session, "session_id"); logicalID != "" {
		body.SessionID = logicalID
	}
	pid := body.ProviderID
	if pid == "" {
		pid = recordString(session, "provider_id")
	}
	if p == nil {
		p, providerID, ok = s.getProvider(pid)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown provider_id: "+pid)
			return
		}
	}
	if storedProvider := recordString(session, "provider_id"); storedProvider != "" && !sameProviderID(storedProvider, providerID) {
		writeError(w, http.StatusConflict, "session_id belongs to provider "+storedProvider+", not "+providerID)
		return
	}
	session["provider_id"] = providerID
	s.bindProviderTranscript(p, body.SessionID)
	attachments, err := s.loadAttachments(providerID, body.SessionID, body.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.activeProvider = providerID
	s.activeSessionID = stringPtr(body.SessionID)
	s.mu.Unlock()

	task := newTaskRecord(s.cfg.DeviceID, body.SessionID, providerID, body.Prompt)
	task["status"] = "sent"
	if err := s.store.AppendTask(task); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !s.beginSend(providerID, body.SessionID) {
		_, _, _ = s.store.UpdateTask(recordString(task, "task_id"), state.Record{
			"status": "error",
			"error":  "send already in progress",
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "task_id": recordString(task, "task_id"), "session_id": body.SessionID,
			"provider_id": providerID, "state": "delivering", "error": "send already in progress",
		})
		return
	}
	session["last_prompt"] = body.Prompt
	// Accepted only means the background provider call has started. Do not
	// expose the session as running until the provider returns a native turn id;
	// otherwise the PWA presents Queue/Insert/Stop for a prompt that Desktop has
	// not received yet.
	session["state"] = "delivering"
	session["updated_at"] = nowISO()
	session["last_error"] = ""
	_ = s.store.UpsertSession(session)
	go s.finishSend(providerID, body.SessionID, body.Prompt, attachments, recordString(task, "task_id"), p)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accepted": true, "task_id": recordString(task, "task_id"), "session_id": body.SessionID,
		"provider_id": providerID, "state": "delivering", "native_session_id": recordString(session, "native_session_id"),
		"title": recordString(session, "title"), "cwd": recordString(session, "cwd"),
		"result": provider.SendResult{OK: true, State: "delivering", Message: "prompt accepted"},
	})
}

func sendKey(providerID string, sessionID string) string {
	return providerID + "\x00" + sessionID
}

func (s *Server) beginSend(providerID string, sessionID string) bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	key := sendKey(providerID, sessionID)
	if s.sendInFlight[key] {
		return false
	}
	s.sendInFlight[key] = true
	return true
}

func (s *Server) endSend(providerID string, sessionID string) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	delete(s.sendInFlight, sendKey(providerID, sessionID))
}

func (s *Server) isSendInFlight(providerID string, sessionID string) bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.sendInFlight[sendKey(providerID, sessionID)]
}

func (s *Server) finishSend(providerID string, sessionID string, prompt string, attachments []provider.Attachment, taskID string, p provider.Provider) {
	defer s.endSend(providerID, sessionID)
	var result provider.SendResult
	if len(attachments) > 0 {
		if sender, ok := p.(provider.AttachmentSender); ok {
			result = sender.SendPromptWithAttachments(sessionID, prompt, attachments)
		} else {
			msg := "provider does not support attachments"
			result = provider.SendResult{OK: false, State: "error", Error: &msg}
		}
	} else {
		result = p.SendPrompt(sessionID, prompt)
	}
	status := "running"
	stateValue := firstNonEmpty(result.State, "running")
	if !result.OK {
		status = "error"
		stateValue = firstNonEmpty(result.State, "error")
	}
	errText := ""
	if result.Error != nil {
		errText = *result.Error
	}
	_, _, _ = s.store.UpdateTask(taskID, state.Record{
		"status":         status,
		"native_task_id": result.NativeTaskID,
		"error":          errText,
	})
	session, ok, err := s.findSessionForProviderAny(providerID, sessionID)
	if err != nil || !ok {
		return
	}
	session["last_prompt"] = prompt
	session["state"] = stateValue
	session["updated_at"] = nowISO()
	if result.OK {
		session["last_error"] = ""
	} else {
		session["last_error"] = firstNonEmpty(errText, "send failed")
	}
	_ = s.store.UpsertSession(session)
}

type rewindUserMessageIn struct {
	ProviderID string `json:"provider_id"`
	SessionID  string `json:"session_id"`
	TurnID     string `json:"turn_id"`
	Prompt     string `json:"prompt"`
	Title      string `json:"title"`
}

func (s *Server) rewindUserMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body rewindUserMessageIn
	if !decodeJSON(w, r, &body) {
		return
	}
	body.SessionID = strings.TrimSpace(body.SessionID)
	body.TurnID = strings.TrimSpace(body.TurnID)
	if body.SessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if err := rejectUnsafeSessionID(body.SessionID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.TurnID == "" {
		writeError(w, http.StatusBadRequest, "turn_id is required")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is empty")
		return
	}
	var rec state.Record
	var found bool
	var err error
	if body.ProviderID != "" {
		rec, found, err = s.findSessionForProviderAny(body.ProviderID, body.SessionID)
	} else {
		rec, found, err = s.findSessionAny(body.SessionID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pid := body.ProviderID
	if pid == "" && found {
		pid = recordString(rec, "provider_id")
	}
	p, providerID, ok := s.getProvider(pid)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id: "+pid)
		return
	}
	if found && recordString(rec, "provider_id") != "" && !sameProviderID(recordString(rec, "provider_id"), providerID) {
		writeError(w, http.StatusConflict, "session_id belongs to provider "+recordString(rec, "provider_id")+", not "+providerID)
		return
	}
	rewinder, ok := p.(provider.UserMessageRewinder)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "detail": "provider does not support message-level rewind/edit"})
		return
	}
	if err := s.hydrateControlSession(p, providerID, body.SessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	logicalID := newID()
	title := firstNonEmpty(body.Title, recordString(rec, "title"))
	if title == "" {
		title = "rewound session"
	}
	cwd := recordString(rec, "cwd")
	task := newTaskRecord(s.cfg.DeviceID, logicalID, providerID, body.Prompt)
	task["status"] = "sent"
	if err := s.store.AppendTask(task); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result, err := rewinder.RewindUserMessage(provider.RewindUserMessageOptions{
		SessionID: logicalID,
		ThreadID:  body.SessionID,
		TurnID:    body.TurnID,
		Prompt:    body.Prompt,
		Cwd:       cwd,
	})
	if err != nil {
		_, _, _ = s.store.UpdateTask(recordString(task, "task_id"), state.Record{
			"status": "error",
			"error":  err.Error(),
		})
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "detail": err.Error(), "task_id": recordString(task, "task_id")})
		return
	}
	threadID := firstNonEmpty(result.ThreadID, body.SessionID)
	_, _, _ = s.store.UpdateTask(recordString(task, "task_id"), state.Record{
		"status":         "running",
		"native_task_id": result.NativeTaskID,
		"error":          "",
	})
	session := newSessionRecord(s.cfg.DeviceID, providerID, title, provider.StartOptions{Cwd: cwd})
	session["session_id"] = logicalID
	session["native_session_id"] = threadID
	session["transcript_id"] = threadID
	session["state"] = firstNonEmpty(result.State, "running")
	session["last_prompt"] = body.Prompt
	session["rewound_from_session_id"] = body.SessionID
	session["rewound_from_turn_id"] = body.TurnID
	session["updated_at"] = nowISO()
	if err := s.store.UpsertSession(session); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.activeProvider = providerID
	s.activeSessionID = stringPtr(logicalID)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "session_id": logicalID, "provider_id": providerID, "thread_id": threadID,
		"transcript_id": threadID, "title": title, "state": firstNonEmpty(result.State, "running"),
		"task_id": recordString(task, "task_id"), "turn_id": result.TurnID, "result": result,
	})
}

type resumeNativeIn struct {
	ProviderID       string `json:"provider_id"`
	NativeSessionID  string `json:"native_session_id"`
	TargetProviderID string `json:"target_provider_id"`
	Fork             bool   `json:"fork"`
}

func (s *Server) nativeSessionByID(providerID string, p provider.Provider, nativeID string) (map[string]any, bool) {
	find := func(rows []map[string]any) (map[string]any, bool) {
		for _, row := range rows {
			if nativeID == stringAny(row["native_session_id"]) || nativeID == stringAny(row["cli_session_id"]) {
				return row, true
			}
		}
		return nil, false
	}
	rows, _ := s.nativeSessionsForProvider(providerID, p, true)
	if row, ok := find(rows); ok {
		return row, true
	}
	s.refreshNativeSessionCacheSync(providerID, p)
	rows, _ = s.nativeSessionsForProvider(providerID, p, false)
	return find(rows)
}

// prepareDirectCodexSession turns a read-only native Codex preview into a
// persisted logical session without calling thread/resume. On first send the
// provider lazily loads that thread in Desktop and targets its IPC owner.
func (s *Server) prepareDirectCodexSession(p provider.Provider, providerID string, nativeID string) (state.Record, error) {
	if err := rejectUnsafeSessionID(nativeID); err != nil {
		return nil, err
	}
	native, ok := s.nativeSessionByID(providerID, p, nativeID)
	if !ok {
		return nil, nil
	}
	threadID := firstNonEmpty(stringAny(native["cli_session_id"]), stringAny(native["native_session_id"]))
	if threadID == "" {
		return nil, fmt.Errorf("native Codex session has no thread id")
	}
	logicalID := providerScopedLogicalID(providerID, threadID)
	if existing, found, err := s.findSessionForProviderAny(providerID, threadID); err != nil {
		return nil, err
	} else if found {
		existing["delivery_route"] = "desktop_ipc"
		if err := s.store.UpsertSession(existing); err != nil {
			return nil, err
		}
		bindSessionTranscript(p, existing, recordString(existing, "session_id"), threadID)
		return existing, nil
	}
	session := newSessionRecord(s.cfg.DeviceID, providerID, firstNonEmpty(stringAny(native["title"]), "Codex"), provider.StartOptions{Cwd: stringAny(native["cwd"])})
	session["session_id"] = logicalID
	session["native_session_id"] = threadID
	session["transcript_id"] = threadID
	session["delivery_route"] = "desktop_ipc"
	if err := s.store.UpsertSession(session); err != nil {
		return nil, err
	}
	bindSessionTranscript(p, session, logicalID, threadID)
	bindSessionTranscript(p, session, nativeID, threadID)
	return session, nil
}

func (s *Server) resumeNativeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body resumeNativeIn
	if !decodeJSON(w, r, &body) {
		return
	}
	src, srcID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id: "+body.ProviderID)
		return
	}
	native, found := s.nativeSessionByID(srcID, src, body.NativeSessionID)
	if !found {
		writeError(w, http.StatusNotFound, "unknown native_session_id: "+body.NativeSessionID)
		return
	}
	cliID := stringAny(native["cli_session_id"])
	if cliID == "" {
		writeError(w, http.StatusBadRequest, "native session has no cli_session_id to activate")
		return
	}
	targetID := body.TargetProviderID
	if targetID == "" {
		targetID = srcID
	}
	target, targetID, ok := s.getProvider(targetID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown target_provider_id: "+targetID)
		return
	}
	resumer, ok := target.(interface {
		OpenResumeSession(sessionID string, resumeID string, cwd string, fork bool) (string, error)
	})
	if !ok {
		writeError(w, http.StatusBadRequest, "target provider cannot activate sessions")
		return
	}
	logicalID := providerScopedLogicalID(targetID, cliID)
	if existing, found, findErr := s.findSessionForProviderAny(targetID, cliID); findErr == nil && found {
		if storedID := recordString(existing, "session_id"); storedID != "" {
			logicalID = storedID
		}
	}
	if body.Fork {
		logicalID = newID()
	}
	if waiter, ok := target.(interface{ WaitResumable(string) bool }); ok && !body.Fork {
		if !waiter.WaitResumable(cliID) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "retry": true, "error": "session turn is still running; retry after it becomes idle"})
			return
		}
	}
	guardKey := targetID + ":" + logicalID
	if !s.acquireResume(guardKey) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "retry": true, "error": "session is already being activated"})
		return
	}
	defer s.releaseResume(guardKey)
	backend, err := resumer.OpenResumeSession(logicalID, cliID, stringAny(native["cwd"]), body.Fork)
	if err != nil || backend == "" {
		errText := "activate failed"
		if err != nil {
			errText = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errText})
		return
	}
	title := firstNonEmpty(stringAny(native["title"]), "activated session")
	if body.Fork {
		title += " (fork)"
	}
	opts := provider.StartOptions{Cwd: stringAny(native["cwd"])}
	session := newSessionRecord(s.cfg.DeviceID, targetID, title, opts)
	session["session_id"] = logicalID
	session["native_session_id"] = backend
	session["transcript_id"] = cliID
	if targetID == "codex" {
		session["transcript_id"] = backend
	}
	session["state"] = "running"
	if targetID == "codex" {
		session["state"] = "idle"
	}
	if err := s.store.UpsertSession(session); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.activeProvider = targetID
	s.activeSessionID = stringPtr(logicalID)
	s.mu.Unlock()
	resp := map[string]any{"ok": true, "session_id": logicalID, "backend_session": backend, "provider_id": targetID, "title": title, "cwd": stringAny(native["cwd"])}
	if targetID == "codex" {
		resp["thread_id"] = backend
	} else {
		resp["tmux_session"] = backend
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) acquireResume(key string) bool {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	if s.resumeInFlight[key] {
		return false
	}
	s.resumeInFlight[key] = true
	return true
}

func (s *Server) releaseResume(key string) {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	delete(s.resumeInFlight, key)
}

func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	p, providerID, ok := s.getProvider(r.URL.Query().Get("provider_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	s.bindProviderTranscript(p, sessionID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider_id": providerID, "output": p.LatestOutput(sessionID)})
}

func (s *Server) nativeSessions(w http.ResponseWriter, r *http.Request) {
	p, providerID, ok := s.getProvider(r.URL.Query().Get("provider_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if queryBool(r.URL.Query().Get("sync")) || r.URL.Query().Get("include_stale") == "0" {
		s.refreshNativeSessionCacheSync(providerID, p)
	}
	sessions, meta := s.nativeSessionsForProvider(providerID, p, true)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id":   providerID,
		"count":         len(sessions),
		"sessions":      sessions,
		"source":        meta.Source,
		"refreshed_at":  meta.RefreshedAt,
		"refreshing":    meta.Refreshing,
		"refresh_error": meta.Error,
	})
}

func (s *Server) nativeSessionsForProvider(providerID string, p provider.Provider, briefWait bool) ([]map[string]any, nativeSessionMeta) {
	done, hadSnapshot := s.ensureNativeSessionRefresh(providerID, p, false)
	if briefWait && !hadSnapshot && done != nil {
		if len(s.storedNativeSessions(providerID)) == 0 {
			select {
			case <-done:
			case <-time.After(nativeSessionBriefWait):
			}
		}
	}
	rows, meta := s.nativeSessionCacheSnapshot(providerID)
	if meta.RefreshedAt == "" {
		meta.Source = "stored"
	} else {
		meta.Source = "cache"
	}
	rows = s.mergeStoredNativeSessions(providerID, rows)
	if meta.Refreshing && meta.Source == "cache" {
		meta.Source = "cache_refreshing"
	}
	return rows, meta
}

func (s *Server) ensureNativeSessionRefresh(providerID string, p provider.Provider, force bool) (<-chan struct{}, bool) {
	if p == nil {
		return nil, false
	}
	s.nativeMu.Lock()
	entry := s.nativeCache[providerID]
	if entry == nil {
		entry = &nativeSessionCacheEntry{}
		s.nativeCache[providerID] = entry
	}
	hadSnapshot := !entry.refreshedAt.IsZero()
	if entry.refreshing {
		done := entry.done
		s.nativeMu.Unlock()
		return done, hadSnapshot
	}
	if !force && hadSnapshot && time.Since(entry.refreshedAt) < nativeSessionRefreshMin {
		s.nativeMu.Unlock()
		return nil, hadSnapshot
	}
	done := make(chan struct{})
	entry.refreshing = true
	entry.done = done
	s.nativeMu.Unlock()
	go s.refreshNativeSessionCache(providerID, p, done)
	return done, hadSnapshot
}

func (s *Server) refreshNativeSessionCacheSync(providerID string, p provider.Provider) {
	done, _ := s.ensureNativeSessionRefresh(providerID, p, true)
	if done != nil {
		<-done
	}
}

func (s *Server) refreshNativeSessionCache(providerID string, p provider.Provider, done chan struct{}) {
	rows := []map[string]any{}
	errText := ""
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				errText = "panic while listing native sessions"
				rows = nil
			}
		}()
		rows = cloneNativeSessionRows(providerID, p.ListNativeSessions())
		sort.Slice(rows, func(i, j int) bool {
			return sessionSortAt(rows[i]) > sessionSortAt(rows[j])
		})
	}()
	s.nativeMu.Lock()
	entry := s.nativeCache[providerID]
	if entry == nil {
		entry = &nativeSessionCacheEntry{}
		s.nativeCache[providerID] = entry
	}
	entry.sessions = rows
	entry.refreshedAt = time.Now()
	entry.refreshing = false
	entry.err = errText
	if entry.done == done {
		entry.done = nil
	}
	close(done)
	s.nativeMu.Unlock()
}

func (s *Server) nativeSessionCacheSnapshot(providerID string) ([]map[string]any, nativeSessionMeta) {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	entry := s.nativeCache[providerID]
	if entry == nil {
		return nil, nativeSessionMeta{}
	}
	meta := nativeSessionMeta{Refreshing: entry.refreshing, Error: entry.err}
	if !entry.refreshedAt.IsZero() {
		meta.RefreshedAt = entry.refreshedAt.UTC().Format(time.RFC3339Nano)
	}
	return cloneNativeSessionRows(providerID, entry.sessions), meta
}

func (s *Server) mergeStoredNativeSessions(providerID string, rows []map[string]any) []map[string]any {
	out := cloneNativeSessionRows(providerID, rows)
	seen := map[string]int{}
	for i, row := range out {
		if key := nativeSessionKey(row); key != "" {
			seen[key] = i
		}
	}
	for _, stored := range s.storedNativeSessions(providerID) {
		key := nativeSessionKey(stored)
		if key == "" {
			continue
		}
		if idx, ok := seen[key]; ok {
			mergeStoredNativeRow(out[idx], stored)
			continue
		}
		seen[key] = len(out)
		out = append(out, stored)
	}
	sort.Slice(out, func(i, j int) bool {
		return sessionSortAt(out[i]) > sessionSortAt(out[j])
	})
	return out
}

func (s *Server) storedNativeSessions(providerID string) []map[string]any {
	records, err := s.store.Sessions()
	if err != nil {
		return nil
	}
	rows := []map[string]any{}
	for _, rec := range records {
		if !sameProviderID(recordString(rec, "provider_id"), providerID) {
			continue
		}
		sessionID := recordString(rec, "session_id")
		transcript := firstNonEmpty(recordString(rec, "transcript_id"), firstNonEmpty(recordString(rec, "native_session_id"), sessionID))
		if transcript == "" {
			continue
		}
		nativeID := firstNonEmpty(recordString(rec, "native_session_id"), transcript)
		rows = append(rows, map[string]any{
			"session_id":        sessionID,
			"cli_session_id":    transcript,
			"native_session_id": nativeID,
			"transcript_id":     transcript,
			"provider_id":       providerID,
			"title":             firstNonEmpty(recordString(rec, "title"), transcript),
			"cwd":               recordString(rec, "cwd"),
			"updated_at":        recordString(rec, "updated_at"),
			"last_reply_at":     recordString(rec, "last_reply_at"),
			"live":              false,
			"status":            firstNonEmpty(recordString(rec, "state"), "stored"),
			"stored":            true,
			"source":            "stored",
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return sessionSortAt(rows[i]) > sessionSortAt(rows[j])
	})
	return rows
}

func cloneNativeSessionRows(providerID string, rows []map[string]any) []map[string]any {
	if len(rows) == 0 {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		cp := map[string]any{}
		for k, v := range row {
			cp[k] = v
		}
		if providerID != "" && stringAny(cp["provider_id"]) == "" {
			cp["provider_id"] = providerID
		}
		if _, ok := cp["stored"]; !ok {
			cp["stored"] = false
		}
		out = append(out, cp)
	}
	return out
}

func nativeSessionKey(row map[string]any) string {
	return firstNonEmpty(
		stringAny(row["cli_session_id"]),
		firstNonEmpty(stringAny(row["native_session_id"]), firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["session_id"]))),
	)
}

func mergeStoredNativeRow(row map[string]any, stored map[string]any) {
	for _, key := range []string{"session_id", "title", "cwd", "updated_at", "last_reply_at", "transcript_id", "native_session_id"} {
		if stringAny(row[key]) == "" && stringAny(stored[key]) != "" {
			row[key] = stored[key]
		}
	}
	if truthy(stored["stored"], false) {
		row["stored"] = true
	}
	if stringAny(row["source"]) == "" {
		row["source"] = stringAny(stored["source"])
	}
}

func (s *Server) sessionOptions(w http.ResponseWriter, r *http.Request) {
	p, providerID, ok := s.getProvider(r.URL.Query().Get("provider_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	status := p.Status()
	defaultCwd := ""
	if status.Cwd != "" {
		if rp, err := realpath(expandUser(status.Cwd)); err == nil {
			defaultCwd = rp
		}
	}
	roots := []map[string]any{}
	seen := map[string]bool{}
	addProjectRoot(&roots, seen, defaultCwd, "default")
	nativeRows, _ := s.nativeSessionsForProvider(providerID, p, true)
	for _, ns := range nativeRows {
		addProjectRoot(&roots, seen, stringAny(ns["cwd"], ns["worktree"]), "recent")
		if len(roots) >= 40 {
			break
		}
	}
	if records, err := s.store.Sessions(); err == nil {
		for _, rec := range records {
			if sameProviderID(recordString(rec, "provider_id"), providerID) {
				addProjectRoot(&roots, seen, recordString(rec, "cwd"), "recent")
			}
		}
	}
	for _, root := range s.browseRoots() {
		addProjectRoot(&roots, seen, root, "root")
	}
	if len(roots) > 60 {
		roots = roots[:60]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "provider_id": providerID, "default_cwd": defaultCwd,
		"roots": roots, "model_select": p.ModelSelect(),
	})
}

func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	target, roots, code, msg := s.safeBrowseDir(r.URL.Query().Get("path"))
	if msg != "" {
		writeError(w, code, msg)
		return
	}
	dirents, err := os.ReadDir(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	entries := []map[string]any{}
	for _, de := range dirents {
		name := de.Name()
		if strings.HasPrefix(name, ".") || projectSkipDirs[name] {
			continue
		}
		info, err := de.Info()
		if err != nil || !info.IsDir() {
			continue
		}
		rp, err := realpath(filepath.Join(target, name))
		if err != nil {
			continue
		}
		ok := false
		for _, root := range roots {
			if under(rp, root) {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		entries = append(entries, map[string]any{"name": name, "path": rp, "mtime": float64(info.ModTime().UnixNano()) / 1e9})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i]["name"].(string)) < strings.ToLower(entries[j]["name"].(string))
	})
	truncated := len(entries) > dirBrowseMax
	if truncated {
		entries = entries[:dirBrowseMax]
	}
	parent := filepath.Dir(target)
	parentAllowed := parent != target
	if parentAllowed {
		parentAllowed = false
		for _, root := range roots {
			if under(parent, root) {
				parentAllowed = true
				break
			}
		}
	}
	if !parentAllowed {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "path": target, "parent": parent, "roots": roots, "entries": entries, "truncated": truncated,
	})
}

func (s *Server) sessionPreview(w http.ResponseWriter, r *http.Request) {
	providerID := r.URL.Query().Get("provider_id")
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if err := rejectUnsafeSessionID(sessionID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, providerID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	s.bindProviderTranscript(p, sessionID)
	if r.URL.Query().Get("usage_only") == "1" {
		model := p.SessionModel(sessionID)
		usage := s.pricing.EnrichUsage(model["usage"])
		writeJSON(w, http.StatusOK, map[string]any{
			"provider_id": providerID, "session_id": sessionID,
			"model": model["model"], "speed": model["speed"], "usage": usage,
		})
		return
	}
	messages, err := p.SessionMessages(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_session_messages failed: "+err.Error())
		return
	}
	s.pricing.EnrichMessages(messages)
	total := len(messages)
	sig := strconv.Itoa(total) + "|"
	if total > 0 {
		last := messages[total-1]
		sig += stringAny(last["kind"]) + ":" + strconv.Itoa(len(stringAny(last["text"]))) + ":" + strconv.Itoa(len(stringAny(last["result"]))) + ":" + stringAny(last["asset_id"])
		if usageJSON, err := json.Marshal(last["usage"]); err == nil && len(usageJSON) > 0 && string(usageJSON) != "null" {
			digest := sha1.Sum(usageJSON)
			sig += ":" + fmt.Sprintf("%x", digest[:8])
		}
	}
	if r.URL.Query().Get("sig_only") == "1" {
		writeJSON(w, http.StatusOK, map[string]any{"provider_id": providerID, "session_id": sessionID, "total": total, "sig": sig})
		return
	}
	offset := -1
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset = parseIntDefault(raw, 0)
	}
	limit := parseIntDefault(r.URL.Query().Get("limit"), 0)
	tail := -1
	if raw := r.URL.Query().Get("tail"); raw != "" {
		tail = parseIntDefault(raw, defaultPreviewTail)
	}
	off := 0
	switch {
	case tail >= 0:
		if tail < 0 {
			tail = 0
		}
		off = total - tail
	case offset >= 0:
		off = offset
	default:
		off = total - defaultPreviewTail
	}
	if off < 0 {
		off = 0
	}
	if off > total {
		off = total
	}
	end := total
	if limit > 0 && off+limit < end {
		end = off + limit
	}
	window := messages[off:end]
	model := map[string]any{}
	if offset < 0 {
		model = p.SessionModel(sessionID)
		if model == nil {
			model = map[string]any{}
		}
		model["usage"] = s.pricing.EnrichUsage(model["usage"])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id": providerID, "session_id": sessionID, "count": len(window),
		"total": total, "offset": off, "sig": sig,
		"model": model["model"], "speed": model["speed"], "ctx_tokens": model["context_tokens"], "out_tokens": model["output_tokens"], "usage": model["usage"],
		"messages": window,
	})
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	path := r.URL.Query().Get("path")
	if sessionID == "" || path == "" {
		writeError(w, http.StatusBadRequest, "session_id and path are required")
		return
	}
	if err := rejectUnsafeSessionID(sessionID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	p, _, ok := s.getProvider(r.URL.Query().Get("provider_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	s.bindProviderTranscript(p, sessionID)
	rp, err := realpath(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	if !p.ReferencedFiles(sessionID)[rp] {
		writeError(w, http.StatusForbidden, "file not referenced in this conversation")
		return
	}
	body, code, msg := fileBody(rp)
	if msg != "" {
		writeError(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	records, err := s.store.Tasks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.reconcileTaskRecords(records)
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
	providerID := strings.TrimSpace(r.URL.Query().Get("provider_id"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if taskID != "" || sessionID != "" || providerID != "" || status != "" {
		filtered := make([]state.Record, 0, len(records))
		for _, rec := range records {
			if taskID != "" && recordString(rec, "task_id") != taskID {
				continue
			}
			if sessionID != "" && recordString(rec, "session_id") != sessionID {
				continue
			}
			if providerID != "" && !sameProviderID(recordString(rec, "provider_id"), providerID) {
				continue
			}
			if status != "" && recordString(rec, "status") != status {
				continue
			}
			filtered = append(filtered, rec)
		}
		records = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": records})
}

// reconcileTaskRecords converges live task rows with the session's actual
// runtime: a task left "running" after its turn completed becomes
// "completed", and a session waiting on an approval shows "waiting_approval".
// Mutates the passed records in place and persists changed rows.
func (s *Server) reconcileTaskRecords(records []state.Record) {
	for _, rec := range records {
		status := recordString(rec, "status")
		if status != "running" && status != "sent" && status != "waiting_approval" && status != "waiting_input" {
			continue
		}
		providerID := canonicalProviderID(recordString(rec, "provider_id"))
		sessionID := recordString(rec, "session_id")
		if providerID == "" || sessionID == "" {
			continue
		}
		p, providerID, _ := s.getProvider(providerID)
		if p == nil || s.isSendInFlight(providerID, sessionID) {
			continue
		}
		s.bindProviderTranscript(p, sessionID)
		stateValue := p.DetectState(sessionID)
		if approvals, ok := p.(interface{ ApprovalRequest(string) map[string]any }); ok {
			if request := approvals.ApprovalRequest(sessionID); request != nil {
				stateValue = pendingInteractionState(request)
			}
		}
		var running *bool
		if scoped, ok := p.(interface{ SessionRunning(string) *bool }); ok {
			running = scoped.SessionRunning(sessionID)
		}
		newStatus := status
		switch {
		case stateValue == "waiting_approval":
			newStatus = "waiting_approval"
		case stateValue == "waiting_input":
			newStatus = "waiting_input"
		case running != nil && *running:
			newStatus = "running"
		case running != nil && !*running:
			newStatus = "completed"
		case stateValue == "idle" || stateValue == "completed":
			newStatus = "completed"
		case stateValue == "running":
			newStatus = "running"
		}
		if newStatus != status {
			rec["status"] = newStatus
			_, _, _ = s.store.UpdateTask(recordString(rec, "task_id"), state.Record{"status": newStatus})
		}
	}
}

type interruptIn struct {
	ProviderID string `json:"provider_id"`
	SessionID  string `json:"session_id"`
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body interruptIn
	if !decodeJSON(w, r, &body) {
		return
	}
	p, providerID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if err := s.hydrateControlSession(p, providerID, body.SessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	res := p.Interrupt(body.SessionID)
	code := http.StatusOK
	if !truthy(res["ok"], false) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
}

type keysIn struct {
	ProviderID string   `json:"provider_id"`
	SessionID  string   `json:"session_id"`
	Keys       []string `json:"keys"`
}

func (s *Server) keys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body keysIn
	if !decodeJSON(w, r, &body) {
		return
	}
	p, providerID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if err := s.hydrateControlSession(p, providerID, body.SessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	res := p.SendKeys(body.SessionID, body.Keys)
	code := http.StatusOK
	if !truthy(res["ok"], false) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
}

type setModelIn struct {
	ProviderID string `json:"provider_id"`
	SessionID  string `json:"session_id"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
}

func (s *Server) setModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body setModelIn
	if !decodeJSON(w, r, &body) {
		return
	}
	p, providerID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if err := s.hydrateControlSession(p, providerID, body.SessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	ms := p.ModelSelect()
	if body.Model != "" && len(ms.Models) > 0 && !modelAllowed(ms.Models, body.Model) {
		writeError(w, http.StatusBadRequest, "unknown model: "+body.Model)
		return
	}
	if body.Effort != "" && len(ms.Efforts) > 0 && !stringIn(ms.Efforts, body.Effort) {
		writeError(w, http.StatusBadRequest, "unknown effort: "+body.Effort)
		return
	}
	writeJSON(w, http.StatusOK, p.SetSessionModel(body.SessionID, body.Model, body.Effort))
}

func (s *Server) steer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body sendPromptIn
	if !decodeJSON(w, r, &body) {
		return
	}
	p, providerID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if err := s.hydrateControlSession(p, providerID, body.SessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	steerer, ok := p.(interface {
		Steer(sessionID string, prompt string) map[string]any
	})
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "detail": "provider does not support steer"})
		return
	}
	res := steerer.Steer(body.SessionID, body.Prompt)
	code := http.StatusOK
	if !truthy(res["ok"], false) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
}

type approvalIn struct {
	TaskID     string `json:"task_id"`
	ProviderID string `json:"provider_id"`
	SessionID  string `json:"session_id"`
	RequestID  string `json:"request_id"`
	Decision   string `json:"decision"`
}

func (s *Server) approval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body approvalIn
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Decision != "allow" && body.Decision != "deny" {
		writeError(w, http.StatusBadRequest, "decision must be 'allow' or 'deny'")
		return
	}
	var task state.Record
	if body.TaskID != "" {
		tasks, err := s.store.Tasks()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, t := range tasks {
			if recordString(t, "task_id") == body.TaskID {
				task = t
				break
			}
		}
	}
	providerID := body.ProviderID
	sessionID := body.SessionID
	if task != nil {
		taskProvider := recordString(task, "provider_id")
		taskSession := recordString(task, "session_id")
		if providerID != "" {
			_, requestedProvider, providerOK := s.getProvider(providerID)
			if !providerOK || (taskProvider != "" && !sameProviderID(requestedProvider, taskProvider)) {
				writeError(w, http.StatusConflict, "task_id provider does not match approval provider")
				return
			}
		}
		if sessionID != "" && taskSession != "" && sessionID != taskSession {
			writeError(w, http.StatusConflict, "task_id session does not match approval session")
			return
		}
		providerID = firstNonEmpty(taskProvider, providerID)
		sessionID = firstNonEmpty(taskSession, sessionID)
	}
	if providerID == "" || sessionID == "" {
		writeError(w, http.StatusNotFound, "approval requires a valid task_id or provider_id/session_id")
		return
	}
	p, resolvedProviderID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	providerID = resolvedProviderID
	if err := s.hydrateControlSession(p, providerID, sessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var relay map[string]any
	if body.RequestID != "" {
		if relayer, ok := p.(interface {
			RelayApprovalRequest(sessionID string, requestID string, decision string) map[string]any
		}); ok {
			relay = relayer.RelayApprovalRequest(sessionID, body.RequestID, body.Decision)
		} else {
			relay = map[string]any{"ok": false, "detail": "provider does not support request-scoped approval"}
		}
	} else {
		relay = p.RelayApproval(sessionID, body.Decision)
	}
	newStatus := "waiting_approval"
	if truthy(relay["ok"], false) {
		if body.RequestID != "" || body.Decision == "allow" {
			newStatus = "running"
		} else {
			newStatus = "idle"
		}
	}
	var updated state.Record
	if task != nil {
		updated, _, _ = s.store.UpdateTask(body.TaskID, state.Record{"status": newStatus, "error": stringAny(relay["detail"])})
	}
	code := http.StatusOK
	if !truthy(relay["ok"], false) {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, map[string]any{
		"ok": truthy(relay["ok"], false), "status": firstNonEmpty(stringAny(relay["status"]), "relayed"),
		"detail": stringAny(relay["detail"]), "request_id": stringAny(relay["request_id"]),
		"decision": body.Decision, "task": updated,
	})
}

type questionAnswerIn struct {
	ProviderID string            `json:"provider_id"`
	SessionID  string            `json:"session_id"`
	RequestID  string            `json:"request_id"`
	Answers    map[string]string `json:"answers"`
}

func (s *Server) questionAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body questionAnswerIn
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.SessionID == "" || body.RequestID == "" || len(body.Answers) == 0 {
		writeError(w, http.StatusBadRequest, "session_id, request_id and answers are required")
		return
	}
	p, providerID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if err := s.hydrateControlSession(p, providerID, body.SessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	answerer, ok := p.(interface {
		AnswerQuestion(sessionID string, requestID string, answers map[string]string) map[string]any
	})
	if !ok {
		writeError(w, http.StatusBadRequest, "provider does not support structured question answers")
		return
	}
	res := answerer.AnswerQuestion(body.SessionID, body.RequestID, body.Answers)
	code := http.StatusOK
	if !truthy(res["ok"], false) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
}

func (s *Server) publishProviderStream(providerID string, target string, frame map[string]any) {
	if target == "" {
		return
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return
	}
	key := approvalIdentity(providerID, target)
	s.streamMu.Lock()
	for ch := range s.streamSubs[key] {
		select {
		case ch <- b:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
	s.streamMu.Unlock()
}

func (s *Server) subscribeStream(providerID string, sessionID string) chan []byte {
	ch := make(chan []byte, 256)
	key := approvalIdentity(providerID, sessionID)
	s.streamMu.Lock()
	if s.streamSubs[key] == nil {
		s.streamSubs[key] = map[chan []byte]bool{}
	}
	s.streamSubs[key][ch] = true
	s.streamMu.Unlock()
	return ch
}

func (s *Server) unsubscribeStream(providerID string, sessionID string, ch chan []byte) {
	key := approvalIdentity(providerID, sessionID)
	s.streamMu.Lock()
	if subs := s.streamSubs[key]; subs != nil {
		delete(subs, ch)
		if len(subs) == 0 {
			delete(s.streamSubs, key)
		}
	}
	s.streamMu.Unlock()
	close(ch)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, "websocket upgrade required")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	providerID := r.URL.Query().Get("provider_id")
	if err := rejectUnsafeSessionID(sessionID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, providerID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if err := s.hydrateControlSession(p, providerID, sessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing websocket key")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "hijacker unavailable")
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_, _ = bufrw.WriteString("Upgrade: websocket\r\n")
	_, _ = bufrw.WriteString("Connection: Upgrade\r\n")
	_, _ = bufrw.WriteString("Sec-WebSocket-Accept: " + websocketAccept(key) + "\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}
	ch := s.subscribeStream(providerID, sessionID)
	defer s.unsubscribeStream(providerID, sessionID, ch)
	running := false
	if sr, ok := p.(interface{ SessionRunning(string) *bool }); ok {
		if v := sr.SessionRunning(sessionID); v != nil {
			running = *v
		}
	}
	hello, _ := json.Marshal(map[string]any{"type": "hello", "provider_id": providerID, "session_id": sessionID, "turn_active": running})
	if err := writeWSFrame(conn, hello); err != nil {
		return
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := writeWSFrame(conn, msg); err != nil {
				return
			}
		case <-ticker.C:
			if err := writeWSFrame(conn, []byte(`{"type":"ping"}`)); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func websocketAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func writeWSFrame(w interface{ Write([]byte) (int, error) }, payload []byte) error {
	header := []byte{0x81}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 65535:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func (s *Server) liveSessions(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.Sessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[string]state.Record{}
	byProviderTranscript := map[string]state.Record{}
	for _, rec := range records {
		sid := recordString(rec, "session_id")
		byID[sid] = rec
		providerID := canonicalProviderID(recordString(rec, "provider_id"))
		for _, tid := range []string{recordString(rec, "transcript_id"), recordString(rec, "native_session_id"), sid} {
			if providerID != "" && tid != "" {
				byProviderTranscript[providerID+":"+tid] = rec
			}
		}
	}
	filterProvider := canonicalProviderID(r.URL.Query().Get("provider_id"))
	includeInactive := queryBool(r.URL.Query().Get("include_inactive"))
	out := []map[string]any{}
	seen := map[string]int{}
	isLiveRow := func(row map[string]any) bool {
		if row["live"] == nil {
			return true
		}
		return truthy(row["live"], false)
	}
	remember := func(key string, row map[string]any) {
		seen[key] = len(out)
		out = append(out, row)
	}
	mergeSeen := func(key string, row map[string]any) bool {
		idx, ok := seen[key]
		if !ok {
			return false
		}
		prev := out[idx]
		if isLiveRow(row) && !isLiveRow(prev) {
			for _, k := range []string{
				"session_id", "transcript_id", "native_session_id", "title", "provider_id", "cwd",
				"updated_at", "last_reply_at", "live", "status", "state", "source", "codex_thread_id",
				"desktop_owner_client_id", "owner_client_id",
			} {
				if v, exists := row[k]; exists && v != nil {
					if k == "live" || stringAny(v) != "" {
						prev[k] = v
					}
				}
			}
			prev["live"] = true
		} else if updated := stringAny(row["updated_at"]); updated > stringAny(prev["updated_at"]) {
			prev["updated_at"] = updated
		}
		return true
	}
	for _, pid := range s.registry.IDs() {
		if filterProvider != "" && pid != filterProvider {
			continue
		}
		runtime, ok := s.registry[pid].(interface{ RuntimeSessions() []map[string]any })
		if ok {
			for _, row := range runtime.RuntimeSessions() {
				sid := stringAny(row["session_id"])
				if sid == "" {
					continue
				}
				runtimeLive := true
				if row["live"] != nil {
					runtimeLive = truthy(row["live"], false)
				}
				if !runtimeLive && !includeInactive {
					continue
				}
				runtimeTranscript := firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["native_session_id"]))
				rec := byID[sid]
				if rec == nil && runtimeTranscript != "" {
					rec = byProviderTranscript[pid+":"+runtimeTranscript]
				}
				if rec == nil {
					rec = state.Record{}
				}
				row["session_id"] = sid
				if recordSid := recordString(rec, "session_id"); recordSid != "" {
					row["session_id"] = recordSid
				}
				row["transcript_id"] = firstNonEmpty(
					recordString(rec, "transcript_id"),
					firstNonEmpty(recordString(rec, "native_session_id"), firstNonEmpty(runtimeTranscript, sid)),
				)
				row["title"] = firstNonEmpty(recordString(rec, "title"), firstNonEmpty(stringAny(row["title"]), sid))
				row["provider_id"] = firstNonEmpty(stringAny(row["provider_id"]), pid)
				row["cwd"] = firstNonEmpty(recordString(rec, "cwd"), stringAny(row["cwd"]))
				row["updated_at"] = firstNonEmpty(stringAny(row["updated_at"]), recordString(rec, "updated_at"))
				row["last_reply_at"] = firstNonEmpty(stringAny(row["last_reply_at"]), recordString(rec, "last_reply_at"))
				row["stored"] = recordString(rec, "session_id") != ""
				if row["live"] == nil {
					row["live"] = true
				}
				seenKey := pid + ":" + firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["session_id"]))
				if mergeSeen(seenKey, row) {
					continue
				}
				remember(seenKey, row)
			}
		}
		if filterProvider == "" {
			continue
		}
		nativeRows, _ := s.nativeSessionsForProvider(pid, s.registry[pid], true)
		for _, native := range nativeRows {
			transcript := firstNonEmpty(stringAny(native["cli_session_id"]), stringAny(native["native_session_id"]))
			if transcript == "" {
				continue
			}
			rec := byProviderTranscript[pid+":"+transcript]
			live := truthy(native["live"], false)
			if !live && !includeInactive {
				continue
			}
			row := map[string]any{
				"session_id":        firstNonEmpty(recordString(rec, "session_id"), transcript),
				"transcript_id":     transcript,
				"native_session_id": firstNonEmpty(stringAny(native["native_session_id"]), transcript),
				"title":             firstNonEmpty(recordString(rec, "title"), firstNonEmpty(stringAny(native["title"]), transcript)),
				"provider_id":       pid,
				"cwd":               firstNonEmpty(recordString(rec, "cwd"), stringAny(native["cwd"])),
				"updated_at":        firstNonEmpty(stringAny(native["updated_at"]), recordString(rec, "updated_at")),
				"last_reply_at":     firstNonEmpty(stringAny(native["last_reply_at"]), recordString(rec, "last_reply_at")),
				"live":              live,
				"status":            stringAny(native["status"]),
				"stored":            recordString(rec, "session_id") != "",
			}
			if live {
				row["state"] = "running"
			}
			if mergeSeen(pid+":"+transcript, row) {
				continue
			}
			remember(pid+":"+transcript, row)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return sessionSortAt(out[i]) > sessionSortAt(out[j])
	})
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

type closeSessionIn struct {
	ProviderID string `json:"provider_id"`
	SessionID  string `json:"session_id"`
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body closeSessionIn
	if !decodeJSON(w, r, &body) {
		return
	}
	requestedProviderID := body.ProviderID
	if body.ProviderID != "" {
		if _, resolved, providerOK := s.getProvider(body.ProviderID); providerOK {
			requestedProviderID = resolved
		}
	}
	rec, ok, err := s.findSessionForProviderAny(requestedProviderID, body.SessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown session_id: "+body.SessionID)
		return
	}
	providerID := canonicalProviderID(recordString(rec, "provider_id"))
	var providerToClose provider.Provider
	if body.ProviderID != "" {
		p, requestedProvider, providerOK := s.getProvider(body.ProviderID)
		if !providerOK || (providerID != "" && !sameProviderID(requestedProvider, providerID)) {
			writeError(w, http.StatusConflict, "session_id does not belong to provider "+body.ProviderID)
			return
		}
		providerToClose = p
		providerID = requestedProvider
	} else if p, _, providerOK := s.getProvider(providerID); providerOK {
		providerToClose = p
	}
	logicalID := recordString(rec, "session_id")
	if providerToClose != nil {
		if err := s.hydrateControlSession(providerToClose, providerID, logicalID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
	}
	if _, removed, removeErr := s.store.RemoveSession(logicalID); removeErr != nil {
		writeError(w, http.StatusInternalServerError, removeErr.Error())
		return
	} else if !removed {
		writeError(w, http.StatusNotFound, "unknown session_id: "+body.SessionID)
		return
	}
	result := map[string]any{"ok": true, "killed": false}
	if providerToClose != nil {
		result = providerToClose.CloseSession(logicalID)
	}
	s.mu.Lock()
	if s.activeSessionID != nil && *s.activeSessionID == logicalID {
		s.activeSessionID = nil
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": truthy(result["ok"], true), "closed": body.SessionID, "provider_result": result})
}

func (s *Server) projectTree(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	root, err := s.sessionProjectRoot(r.URL.Query().Get("provider_id"), sessionID)
	if err != nil {
		writeErrorFromErr(w, err)
		return
	}
	target, err := projectPath(root, r.URL.Query().Get("path"))
	if err != nil {
		writeErrorFromErr(w, err)
		return
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is not a directory")
		return
	}
	dirents, err := os.ReadDir(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	entries := []map[string]any{}
	for _, de := range dirents {
		name := de.Name()
		if projectSkipDirs[name] {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		rp, err := realpath(filepath.Join(target, name))
		if err != nil || (rp != root && !under(rp, root)) {
			continue
		}
		rel, _ := filepath.Rel(root, rp)
		row := map[string]any{
			"name": name, "path": rel, "mtime": float64(info.ModTime().UnixNano()) / 1e9,
		}
		if info.IsDir() {
			row["type"] = "dir"
			row["size"] = nil
		} else {
			row["type"] = "file"
			row["size"] = info.Size()
		}
		entries = append(entries, row)
	}
	sortDirEntries(entries)
	truncated := len(entries) > projectMaxEntries
	if truncated {
		entries = entries[:projectMaxEntries]
	}
	path := ""
	if target != root {
		path, _ = filepath.Rel(root, target)
	}
	_, providerID, _ := s.getProvider(r.URL.Query().Get("provider_id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "provider_id": providerID, "session_id": sessionID, "root": root,
		"path": path, "entries": entries, "truncated": truncated,
	})
}

func (s *Server) projectFile(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	path := r.URL.Query().Get("path")
	if sessionID == "" || path == "" {
		writeError(w, http.StatusBadRequest, "session_id and path are required")
		return
	}
	root, err := s.sessionProjectRoot(r.URL.Query().Get("provider_id"), sessionID)
	if err != nil {
		writeErrorFromErr(w, err)
		return
	}
	target, err := projectPath(root, path)
	if err != nil {
		writeErrorFromErr(w, err)
		return
	}
	body, code, msg := fileBody(target)
	if msg != "" {
		writeError(w, code, msg)
		return
	}
	body["root"] = root
	body["relpath"], _ = filepath.Rel(root, target)
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) gitLog(w http.ResponseWriter, r *http.Request) {
	root, repo, ok := s.gitRepoForSession(w, r)
	if !ok {
		return
	}
	_ = root
	limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	out, err := runGit(repo, 8*time.Second, "log", "--max-count="+strconv.Itoa(limit), "--pretty=format:%H%x1f%h%x1f%ct%x1f%an%x1f%s")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	commits := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 5)
		if len(parts) != 5 {
			continue
		}
		ts, _ := strconv.Atoi(parts[2])
		commits = append(commits, map[string]any{"hash": parts[0], "short": parts[1], "timestamp": ts, "author": parts[3], "subject": parts[4]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "root": repo, "count": len(commits), "commits": commits})
}

func (s *Server) gitCommit(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.gitRepoForSession(w, r)
	if !ok {
		return
	}
	commit := strings.TrimSpace(r.URL.Query().Get("commit"))
	if !commitRE.MatchString(commit) {
		writeError(w, http.StatusBadRequest, "invalid commit")
		return
	}
	if _, err := runGit(repo, 4*time.Second, "cat-file", "-e", commit+"^{commit}"); err != nil {
		writeError(w, http.StatusNotFound, "commit not found")
		return
	}
	meta, err := runGit(repo, 8*time.Second, "show", "-s", "--format=%H%x1f%h%x1f%ct%x1f%an%x1f%ae%x1f%P%x1f%B", commit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	filesOut, err := runGit(repo, 8*time.Second, "diff-tree", "--no-commit-id", "--name-status", "-r", "-M", commit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	patch, err := runGit(repo, 10*time.Second, "show", "--no-ext-diff", "--find-renames", "--format=", "--patch", "--no-color", "--unified=80", commit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	parts := strings.SplitN(meta, "\x1f", 7)
	if len(parts) != 7 {
		writeError(w, http.StatusInternalServerError, "git commit metadata parse failed")
		return
	}
	ts, _ := strconv.Atoi(parts[2])
	files := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(filesOut), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		status := fields[0]
		path := ""
		oldPath := ""
		if len(fields) > 1 {
			path = fields[len(fields)-1]
		}
		if len(fields) > 2 {
			oldPath = fields[1]
		}
		files = append(files, map[string]any{"status": status, "path": path, "old_path": oldPath})
	}
	truncated := len(patch) > gitPatchMaxChars
	if truncated {
		patch = patch[:gitPatchMaxChars]
	}
	message := strings.TrimSpace(parts[6])
	subject := ""
	if lines := strings.Split(message, "\n"); len(lines) > 0 {
		subject = lines[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "root": repo,
		"commit": map[string]any{
			"hash": parts[0], "short": parts[1], "timestamp": ts, "author": parts[3],
			"email": parts[4], "parents": splitFields(parts[5]), "subject": subject,
			"message": message, "files": files, "patch": patch, "patch_truncated": truncated,
		},
	})
}

func (s *Server) gitRepoForSession(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return "", "", false
	}
	root, err := s.sessionProjectRoot(r.URL.Query().Get("provider_id"), sessionID)
	if err != nil {
		writeErrorFromErr(w, err)
		return "", "", false
	}
	repo := gitTopLevel(root)
	if repo == "" {
		writeError(w, http.StatusNotFound, "project is not a git repository")
		return "", "", false
	}
	return root, repo, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "detail": msg})
}

func writeErrorFromErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	code := http.StatusInternalServerError
	switch {
	case strings.Contains(msg, "invalid"):
		code = http.StatusBadRequest
	case strings.Contains(msg, "outside"):
		code = http.StatusForbidden
	case strings.Contains(msg, "not found"):
		code = http.StatusNotFound
	case strings.Contains(msg, "required"):
		code = http.StatusBadRequest
	}
	writeError(w, code, msg)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func stringPtr(v string) *string { return &v }

func nullableActiveSession(activeProvider string, providerID string, activeSessionID *string) *string {
	if activeProvider != providerID {
		return nil
	}
	return activeSessionID
}

func stringAny(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func truthy(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func queryBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func sessionSortAt(row map[string]any) string {
	return firstNonEmpty(stringAny(row["last_reply_at"]), stringAny(row["updated_at"]))
}

func splitFields(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}
