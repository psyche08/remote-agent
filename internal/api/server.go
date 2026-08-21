package api

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/pricing"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
	webui "github.com/psyche08/remote-agent/static"
)

type Server struct {
	cfg              *config.Config
	registry         provider.Registry
	store            *state.Store
	activeProvider   string
	activeSessionID  *string
	mu               sync.Mutex
	resumeMu         sync.Mutex
	resumeInFlight   map[string]bool
	sendMu           sync.Mutex
	sendInFlight     map[string]string
	streamMu         sync.Mutex
	streamSubs       map[string]map[chan []byte]bool
	presenceMu       sync.Mutex
	presence         map[string]time.Time
	pushMu           sync.Mutex
	pushLast         map[string]string
	pushStop         chan struct{}
	pushOnce         sync.Once
	pushSender       func(map[string]any) int
	updateMu         sync.Mutex
	updateRunning    bool
	nativeMu         sync.Mutex
	nativeCache      map[string]*nativeSessionCacheEntry
	clientMu         sync.Mutex
	clients          map[string]*clientVersionSeen
	pricing          *pricing.Manager
	lastScreenshot   string
	lastShotAt       string
	computerUseCtl   *computeruse.Controller
	computerUseMu    sync.Mutex
	computerUseTurns map[computerUseTurnKey]computerUseTurnLease
	computerUseEnded map[computerUseTurnIdentity]struct{}
	// Test-only barrier invoked after a trusted provider callback is made
	// permanently stale and before its synchronous close/relock begins.
	computerUseAutomationRevokedHook func()
}

type nativeSessionCacheEntry struct {
	sessions    []map[string]any
	refreshedAt time.Time
	generation  uint64
	refreshing  bool
	done        chan struct{}
	err         string
}

type nativeSessionMeta struct {
	Source      string
	RefreshedAt string
	Generation  uint64
	HasSnapshot bool
	Refreshing  bool
	Error       string
}

// nativeSessionErrorLister lets providers surface discovery failures without
// changing the Provider compatibility contract. Providers that only implement
// ListNativeSessions retain the existing behavior.
type nativeSessionErrorLister interface {
	ListNativeSessionsWithError() ([]map[string]any, error)
}

const (
	nativeSessionRefreshDefault = 15 * time.Second
	nativeSessionRefreshCodex   = 5 * time.Second
	nativeSessionBriefWait      = 150 * time.Millisecond
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
		resumeInFlight: map[string]bool{}, sendInFlight: map[string]string{},
		streamSubs: map[string]map[chan []byte]bool{}, presence: map[string]time.Time{},
		pushLast: map[string]string{}, pushStop: make(chan struct{}), nativeCache: map[string]*nativeSessionCacheEntry{},
		clients: map[string]*clientVersionSeen{}, pricing: pricing.New(store.DataDir()),
		computerUseTurns: map[computerUseTurnKey]computerUseTurnLease{},
		computerUseEnded: map[computerUseTurnIdentity]struct{}{},
	}
	s.pushSender = func(payload map[string]any) int { return s.sendPushToAll(payload, true) }
	// The controller exists whenever computer use is configured on, so the
	// status route can explain the feature. Locked Use only arms in
	// StartBackground, after its startup scrub establishes a locked baseline.
	if cfg.ComputerUse.Enabled {
		s.computerUseCtl = computeruse.NewController(
			expandUser(cfg.ComputerUse.HelperSocket),
			cfg.ComputerUse.Enabled, cfg.ComputerUse.LockedUse.Enabled,
		)
	}
	for _, id := range registry.IDs() {
		providerID := id
		registeredProvider := registry[id]
		if p, ok := registeredProvider.(interface {
			SetStreamPublisher(func(target string, frame map[string]any))
		}); ok {
			p.SetStreamPublisher(func(target string, frame map[string]any) {
				s.publishProviderStream(providerID, target, frame)
			})
		}
		if host, ok := registeredProvider.(provider.ClaudeControlRouteCommitHost); ok {
			s.installClaudeControlRouteCommitHandler(providerID, host)
		}
		if s.computerUseCtl != nil {
			if host, ok := registeredProvider.(provider.ComputerUseToolHost); ok {
				s.installComputerUseToolHandler(providerID, host)
			}
			if host, ok := registeredProvider.(provider.ComputerUseAutomationHost); ok {
				s.installComputerUseAutomationHandler(providerID, host)
			}
		}
	}
	// A running/waiting state belongs to one AgentHalo process generation.
	// Never carry it across a restart; live provider discovery will repopulate
	// authoritative runtime state after startup.
	s.resetPersistedTransientSessions()
	return s
}

func (s *Server) resetPersistedTransientSessions() {
	records, err := s.store.Sessions()
	if err != nil {
		return
	}
	changed := false
	for _, rec := range records {
		switch recordString(rec, "state") {
		case "running", "delivering", "waiting_approval", "waiting_input", "interrupting":
			rec["state"] = "idle"
			changed = true
		}
	}
	if changed {
		_ = s.store.SaveSessions(records)
	}
}

func (s *Server) StartBackground() {
	s.StartBackgroundWithAutoUpdate(true)
}

func (s *Server) StartBackgroundWithAutoUpdate(autoUpdate bool) {
	s.StartBackgroundWithOptions(autoUpdate, true)
}

func (s *Server) StartBackgroundWithOptions(autoUpdate bool, watchdog bool) {
	s.pushOnce.Do(func() {
		go s.pushMonitorLoop()
		if autoUpdate {
			go s.autoUpdateLoop()
		}
		if watchdog {
			go s.watchdogLoop()
		}
		if s.computerUseCtl != nil {
			s.computerUseCtl.Start()
		}
		s.pricing.Start(s.pushStop)
	})
}

func (s *Server) StopBackground() {
	if s.computerUseCtl != nil {
		// Closing the window relocks the screen before the process goes away.
		// A shutdown must not be a way to leave a Mac unlocked.
		s.computerUseCtl.Stop()
	}
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
	mux.HandleFunc("/computer_use", s.computerUse)
	mux.HandleFunc("/computer_use/locked_use", s.computerUseLockedUse)
	mux.HandleFunc("/computer_use/window", s.computerUseWindow)
	mux.HandleFunc("/computer_use/action", s.computerUseAction)
	mux.HandleFunc("/computer_use/ax", s.computerUseAX)
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
	// through /s/agenthalo/d/<device>/ without leaving the root PWA URL.
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
	if sidView != "" {
		if sendState := s.sendState(providerID, sidView); sendState != "" {
			stateValue = sendState
		}
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
			"actions":      provider.Actions(p),
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
		records = s.visibleStoredSessions(
			records,
			strings.TrimSpace(r.URL.Query().Get("provider_id")),
			strings.TrimSpace(r.URL.Query().Get("session_id")),
		)
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
			if providerID == "codex" {
				setCodexAppServerDeliveryRoute(session, p)
				bindSessionTranscript(p, session, recordString(session, "session_id"), native)
			}
		}
		if providerID == "claude" {
			// Snapshot route plus monotonic commitment only after the provider
			// operation and native identity (if any) are known.
			setClaudeControlRoute(session, p, recordString(session, "session_id"))
			if native != "" {
				bindSessionTranscript(p, session, recordString(session, "session_id"), native)
			}
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
	OperationID string   `json:"operation_id"`
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
	// transcript id. Route it through prepareDirectCodexSession even when the
	// record lookup succeeded so the provider's current native delivery route
	// is persisted before any prompt is accepted.
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
	body.OperationID = strings.TrimSpace(body.OperationID)
	if body.OperationID == "" {
		if providerID == "claude" {
			writeError(w, http.StatusBadRequest, "operation_id is required for restart-safe Claude delivery")
			return
		}
		body.OperationID = newID()
	} else if err := validatePromptOperationID(body.OperationID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	session["provider_id"] = providerID
	// Migrate persisted native Codex sessions before binding. Older releases
	// recorded desktop_ipc even on ChatGPT Desktop builds that never publish
	// an owner to the private VS Code IPC router. Route selection is explicit
	// and happens before prompt delivery, so this is not a retry/fallback.
	if providerID == "codex" && recordString(session, "codex_control_route") == "" &&
		codexDesktopDeliveryRecord(session) {
		route := codexPreferredNativeDeliveryRoute(p)
		// Keep delivery_route=desktop_ipc as a fail-closed rollback contract
		// for pre-shared-daemon binaries. New binaries always persist and use
		// the explicit control route, including Desktop compatibility mode.
		session["codex_control_route"] = route
		if err := s.store.UpsertSession(session); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.bindProviderTranscript(p, body.SessionID)
	// Route-less Claude records predate Computer Use ownership persistence.
	// Adopt the provider's default only after binding; an existing canonical
	// route remains authoritative and must not be replaced by a fresh-process
	// default before delivery.
	if providerID == "claude" && canonicalClaudeControlRoute(recordString(session, "claude_control_route")) == "" &&
		setClaudeControlRoute(session, p, body.SessionID) {
		if err := s.store.UpsertSession(session); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	attachments, err := s.loadAttachments(providerID, body.SessionID, body.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.activeProvider = providerID
	s.activeSessionID = stringPtr(body.SessionID)
	s.mu.Unlock()

	deliveryState := "delivering"
	if codexDesktopDeliveryForProvider(p, session) {
		deliveryState = "attaching"
	}
	requestDigest := promptRequestDigest(providerID, body.SessionID, body.Prompt, body.Attachments)
	task := newTaskRecordWithID(
		s.cfg.DeviceID, body.SessionID, providerID, body.Prompt, body.OperationID,
	)
	task["operation_id"] = body.OperationID
	task["request_digest"] = requestDigest
	if deliveryState == "attaching" {
		task["status"] = "attaching"
	} else {
		task["status"] = "sent"
	}
	durableTask, created, err := s.store.AppendTaskOnce(task)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !created {
		if recordString(durableTask, "request_digest") != requestDigest ||
			!sameProviderID(recordString(durableTask, "provider_id"), providerID) ||
			recordString(durableTask, "session_id") != body.SessionID {
			writeError(w, http.StatusConflict, "operation_id is already bound to another prompt request")
			return
		}
		task = durableTask
		switch recordString(task, "status") {
		case "running", "completed", "waiting_approval", "waiting_input":
			repairedSession, repairErr := s.repairPromptOperationBinding(p, providerID, task)
			if repairErr != nil {
				writeError(w, http.StatusInternalServerError, repairErr.Error())
				return
			}
			session = repairedSession
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "accepted": true, "idempotent": true,
				"task_id": body.OperationID, "session_id": body.SessionID,
				"provider_id": providerID, "state": recordString(task, "status"),
				"native_session_id": recordString(session, "native_session_id"),
				"title":             recordString(session, "title"), "cwd": recordString(session, "cwd"),
			})
			return
		case "needs_manual", "delivery_unknown":
			repairedSession, repairErr := s.repairPromptOperationBinding(p, providerID, task)
			if repairErr != nil {
				writeError(w, http.StatusInternalServerError, repairErr.Error())
				return
			}
			session = repairedSession
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "accepted": false, "idempotent": true,
				"task_id": body.OperationID, "session_id": body.SessionID,
				"provider_id": providerID, "state": "needs_manual",
				"delivery_outcome":  "unknown",
				"native_session_id": recordString(session, "native_session_id"),
				"error":             firstNonEmpty(recordString(task, "error"), "prompt delivery is uncertain and requires manual review"),
			})
			return
		case "error":
			repairedSession, repairErr := s.repairPromptOperationBinding(p, providerID, task)
			if repairErr != nil {
				writeError(w, http.StatusInternalServerError, repairErr.Error())
				return
			}
			session = repairedSession
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "accepted": false, "idempotent": true,
				"task_id": body.OperationID, "session_id": body.SessionID,
				"provider_id": providerID, "state": "error",
				"error": firstNonEmpty(recordString(task, "error"), "prompt delivery failed"),
			})
			return
		}
	}
	if !s.beginSend(providerID, body.SessionID, deliveryState) {
		if !created {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "accepted": true, "idempotent": true,
				"task_id": body.OperationID, "session_id": body.SessionID,
				"provider_id": providerID, "state": firstNonEmpty(recordString(task, "status"), deliveryState),
				"native_session_id": recordString(session, "native_session_id"),
				"title":             recordString(session, "title"), "cwd": recordString(session, "cwd"),
			})
			return
		}
		updatedTask, _, _ := s.store.UpdateTask(recordString(task, "task_id"), state.Record{
			"status": "error",
			"error":  "send already in progress",
		})
		s.publishTaskStream(providerID, body.SessionID, updatedTask)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "task_id": recordString(task, "task_id"), "session_id": body.SessionID,
			"provider_id": providerID, "state": firstNonEmpty(s.sendState(providerID, body.SessionID), deliveryState), "error": "send already in progress",
		})
		return
	}
	s.publishTaskStream(providerID, body.SessionID, task)
	session["last_prompt"] = body.Prompt
	// Accepted only means the background provider call has started. Do not
	// expose the session as running until the provider returns a native turn id;
	// otherwise the PWA presents Queue/Insert/Stop for a prompt that Desktop has
	// not received yet.
	session["state"] = deliveryState
	session["updated_at"] = nowISO()
	session["last_error"] = ""
	_ = s.store.UpsertSession(session)
	go s.finishSend(providerID, body.SessionID, body.Prompt, attachments, recordString(task, "task_id"), p)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accepted": true, "task_id": recordString(task, "task_id"), "session_id": body.SessionID,
		"provider_id": providerID, "state": deliveryState, "native_session_id": recordString(session, "native_session_id"),
		"title": recordString(session, "title"), "cwd": recordString(session, "cwd"),
		"result": provider.SendResult{OK: true, State: deliveryState, Message: "prompt accepted"},
	})
}

func sendKey(providerID string, sessionID string) string {
	return providerID + "\x00" + sessionID
}

func (s *Server) beginSend(providerID string, sessionID string, stateValue string) bool {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	key := sendKey(providerID, sessionID)
	if s.sendInFlight[key] != "" {
		return false
	}
	s.sendInFlight[key] = stateValue
	return true
}

func (s *Server) endSend(providerID string, sessionID string) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	delete(s.sendInFlight, sendKey(providerID, sessionID))
}

func (s *Server) isSendInFlight(providerID string, sessionID string) bool {
	return s.sendState(providerID, sessionID) != ""
}

func (s *Server) sendState(providerID string, sessionID string) string {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.sendInFlight[sendKey(providerID, sessionID)]
}

func (s *Server) finishSend(providerID string, sessionID string, prompt string, attachments []provider.Attachment, taskID string, p provider.Provider) {
	defer s.endSend(providerID, sessionID)
	defer func() {
		if recovered := recover(); recovered != nil {
			s.recordPromptProviderPanic(providerID, sessionID, taskID)
		}
	}()

	var result provider.SendResult
	if len(attachments) > 0 {
		if sender, ok := p.(provider.PromptOperationAttachmentSender); ok {
			result = sender.SendPromptOperationWithAttachments(sessionID, prompt, attachments, taskID)
		} else if sender, ok := p.(provider.AttachmentSender); ok {
			result = sender.SendPromptWithAttachments(sessionID, prompt, attachments)
		} else {
			msg := "provider does not support attachments"
			result = provider.SendResult{OK: false, State: "error", Error: &msg}
		}
	} else {
		if sender, ok := p.(provider.PromptOperationSender); ok {
			result = sender.SendPromptOperation(sessionID, prompt, taskID)
		} else {
			result = p.SendPrompt(sessionID, prompt)
		}
	}
	status, stateValue, deliveryOutcome := promptDeliveryOutcome(result)
	errText := ""
	if result.Error != nil {
		errText = *result.Error
	}
	claudeRoute := ""
	claudeCommitted, claudeCommittedKnown := false, false
	if providerID == "claude" {
		// Snapshot provider-owned state before entering the store transaction.
		// Provider callbacks may synchronously acquire the store, so calling back
		// into the provider while holding the store mutex would invert that lock
		// order.
		claudeRoute = claudeControlRouteFromProvider(p, sessionID)
		claudeCommitted, claudeCommittedKnown = claudeControlCommittedFromProvider(p, sessionID)
	}
	updatedTask, updatedSession, found, commitErr := s.store.CommitTaskSessionOutcome(
		taskID, sessionID, func(task state.Record, session state.Record) error {
			if !sameProviderID(recordString(task, "provider_id"), providerID) {
				return errors.New("provider task ownership changed during send")
			}
			if !sameProviderID(recordString(session, "provider_id"), providerID) {
				return errors.New("provider session ownership changed during send")
			}
			task["status"] = status
			task["delivery_outcome"] = deliveryOutcome
			task["native_task_id"] = result.NativeTaskID
			task["error"] = errText
			session["last_prompt"] = prompt
			session["state"] = stateValue
			if result.OK {
				session["last_error"] = ""
			} else {
				session["last_error"] = firstNonEmpty(errText, "send failed")
			}
			if result.NativeSessionID != "" {
				// Desktop-first Claude sessions may not have a native transcript until
				// their first UI send creates one. Persist and bind that identity before
				// publishing the completed delivery state so later approvals, answers,
				// and restart hydration target the same conversation.
				session["native_session_id"] = result.NativeSessionID
				session["transcript_id"] = result.NativeSessionID
			}
			if providerID == "claude" {
				// A pre-mutation Computer Use failure may intentionally switch this
				// logical session to the CLI fallback. Publish that owner and its
				// monotonic commitment in the same transaction as the task outcome.
				if claudeRoute != "" {
					session["claude_control_route"] = claudeRoute
				}
				committed := claudeControlCommittedForBinding(
					session, firstNonEmpty(recordString(session, "transcript_id"), recordString(session, "native_session_id")),
				)
				if claudeRoute == "desktop_computer_use" && claudeCommittedKnown {
					committed = committed || claudeCommitted
				}
				session[claudeControlCommittedKey] = committed
				task["claude_control_route"] = recordString(session, "claude_control_route")
				task[claudeControlCommittedKey] = committed
			}
			task["native_session_id"] = recordString(session, "native_session_id")
			task["transcript_id"] = recordString(session, "transcript_id")
			return nil
		})
	if commitErr != nil || !found {
		// A provider call has already crossed its side-effect boundary. Never
		// turn a storage failure into a definite delivery failure: first replay a
		// prepared journal, then expose only an in-memory needs_manual outcome if
		// no durable result can be recovered. A retry with this operation id will
		// let the provider's tombstone decide whether delivery was attempted.
		if recoveredTask, recoveredSession, recovered := s.persistedPromptOperationOutcome(taskID, sessionID); recovered {
			updatedTask, updatedSession, found = recoveredTask, recoveredSession, true
		} else {
			updatedTask = state.Record{
				"task_id": taskID, "session_id": sessionID, "provider_id": providerID,
				"status": "needs_manual", "delivery_outcome": "unknown",
				"error": "prompt delivery outcome could not be durably recorded; manual review is required",
			}
			if current, ok, err := s.findSessionForProviderAny(providerID, sessionID); err == nil && ok {
				updatedSession = current
			}
		}
	}
	if found && updatedSession != nil && providerID == "claude" {
		transcriptID := firstNonEmpty(
			recordString(updatedSession, "transcript_id"), recordString(updatedSession, "native_session_id"),
		)
		bindSessionTranscript(p, updatedSession, sessionID, transcriptID)
	}
	s.publishTaskStream(providerID, sessionID, updatedTask)
}

func (s *Server) recordPromptProviderPanic(providerID string, sessionID string, taskID string) {
	const detail = "provider stopped unexpectedly after prompt admission; delivery outcome is unknown and requires manual review"
	updatedTask, _, found, err := s.store.CommitTaskSessionOutcome(
		taskID, sessionID, func(task state.Record, session state.Record) error {
			if !sameProviderID(recordString(task, "provider_id"), providerID) ||
				!sameProviderID(recordString(session, "provider_id"), providerID) {
				return errors.New("provider ownership changed while recording an uncertain prompt outcome")
			}
			task["status"] = "needs_manual"
			task["delivery_outcome"] = "unknown"
			task["error"] = detail
			session["state"] = "needs_manual"
			session["last_error"] = detail
			return nil
		},
	)
	if err != nil || !found {
		if recoveredTask, _, recovered := s.persistedPromptOperationOutcome(taskID, sessionID); recovered {
			updatedTask = recoveredTask
		} else {
			updatedTask = state.Record{
				"task_id": taskID, "session_id": sessionID, "provider_id": providerID,
				"status": "needs_manual", "delivery_outcome": "unknown", "error": detail,
			}
		}
	}
	s.publishTaskStream(providerID, sessionID, updatedTask)
}

func promptDeliveryOutcome(result provider.SendResult) (taskStatus string, sessionState string, outcome string) {
	stateValue := strings.TrimSpace(result.State)
	if result.OK {
		switch stateValue {
		case "idle", "completed":
			return "completed", firstNonEmpty(stateValue, "completed"), "confirmed"
		case "waiting_approval", "waiting_input":
			return stateValue, stateValue, "confirmed"
		default:
			return "running", firstNonEmpty(stateValue, "running"), "confirmed"
		}
	}
	if stateValue == "needs_manual" || stateValue == "delivery_unknown" {
		return "needs_manual", "needs_manual", "unknown"
	}
	return "error", firstNonEmpty(stateValue, "error"), "failed"
}

func (s *Server) persistedPromptOperationOutcome(taskID string, sessionID string) (state.Record, state.Record, bool) {
	tasks, err := s.store.Tasks()
	if err != nil {
		return nil, nil, false
	}
	var task state.Record
	for _, candidate := range tasks {
		if recordString(candidate, "task_id") == taskID && recordString(candidate, "session_id") == sessionID {
			task = candidate
			break
		}
	}
	if task == nil || recordString(task, "delivery_outcome") == "" {
		return nil, nil, false
	}
	session, found, err := s.store.FindSession(sessionID)
	if err != nil || !found {
		return nil, nil, false
	}
	return task, session, true
}

// repairPromptOperationBinding makes a terminal duplicate self-healing. The
// task carries the native transcript and Claude owner published with its
// outcome; if a legacy or externally interrupted write left the session
// incomplete, the same journaled transaction restores the missing half before
// the provider is rebound. Conflicting proof fails closed and is never sent.
func (s *Server) repairPromptOperationBinding(
	p provider.Provider, providerID string, durableTask state.Record,
) (state.Record, error) {
	taskID := recordString(durableTask, "task_id")
	sessionID := recordString(durableTask, "session_id")
	_, repairedSession, found, err := s.store.CommitTaskSessionOutcome(
		taskID, sessionID, func(task state.Record, session state.Record) error {
			if !sameProviderID(recordString(task, "provider_id"), providerID) ||
				!sameProviderID(recordString(session, "provider_id"), providerID) {
				return errors.New("prompt operation binding belongs to another provider")
			}
			taskTranscript := recordString(task, "transcript_id")
			taskNative := recordString(task, "native_session_id")
			if taskTranscript != "" && taskNative != "" && taskTranscript != taskNative {
				return errors.New("prompt operation contains conflicting native session proof")
			}
			proof := firstNonEmpty(taskTranscript, taskNative)
			sessionTranscript := recordString(session, "transcript_id")
			sessionNative := recordString(session, "native_session_id")
			if proof == "" {
				if sessionTranscript != "" && sessionNative != "" && sessionTranscript != sessionNative {
					return errors.New("stored session contains conflicting native session proof")
				}
				proof = firstNonEmpty(sessionTranscript, sessionNative)
			}
			if proof != "" {
				if (sessionTranscript != "" && sessionTranscript != proof) ||
					(sessionNative != "" && sessionNative != proof) {
					return errors.New("prompt operation native session proof conflicts with stored session")
				}
				task["transcript_id"], task["native_session_id"] = proof, proof
				session["transcript_id"], session["native_session_id"] = proof, proof
			}
			if providerID == "claude" {
				taskRoute := canonicalClaudeControlRoute(recordString(task, "claude_control_route"))
				sessionRoute := canonicalClaudeControlRoute(recordString(session, "claude_control_route"))
				if taskRoute != "" && sessionRoute != "" && taskRoute != sessionRoute {
					return errors.New("prompt operation Claude route conflicts with stored session")
				}
				route := firstNonEmpty(taskRoute, sessionRoute)
				if route != "" {
					task["claude_control_route"], session["claude_control_route"] = route, route
				}
				committed := claudeControlCommittedForBinding(session, proof)
				if taskCommitted, ok := task[claudeControlCommittedKey].(bool); ok {
					committed = committed || taskCommitted
				}
				task[claudeControlCommittedKey], session[claudeControlCommittedKey] = committed, committed
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("repair prompt operation binding: %w", err)
	}
	if !found {
		return nil, errors.New("repair prompt operation binding: durable task or session is missing")
	}
	transcriptID := firstNonEmpty(
		recordString(repairedSession, "transcript_id"), recordString(repairedSession, "native_session_id"),
	)
	if transcriptID != "" || providerID == "claude" {
		bindSessionTranscript(p, repairedSession, sessionID, transcriptID)
	}
	return repairedSession, nil
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
	if providerID == "codex" {
		setCodexAppServerDeliveryRoute(session, p)
		bindSessionTranscript(p, session, logicalID, threadID)
	}
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
// persisted logical session without delivering a prompt. The provider's
// explicit native route determines whether first send resumes the thread on
// app-server or addresses a known Desktop IPC owner.
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
	hidden := sessionHiddenFromLists(native, nil)
	if !hidden {
		hidden = sessionHiddenFromLists(native, s.hiddenSessionIDs(providerID))
	}
	logicalID := providerScopedLogicalID(providerID, threadID)
	if existing, found, err := s.findSessionForProviderAny(providerID, threadID); err != nil {
		return nil, err
	} else if found {
		if recordString(existing, "codex_control_route") == "" {
			controlRoute := codexControlRouteForProvider(p, existing)
			if controlRoute == "app_server" {
				controlRoute = codexAppServerDeliveryRoute(p)
			}
			existing["codex_control_route"] = controlRoute
			switch controlRoute {
			case "shared_daemon", "desktop_ipc":
				existing["delivery_route"] = "desktop_ipc"
			case "stdio":
				delete(existing, "delivery_route")
			}
		}
		if hidden {
			existing[hiddenFromSessionListsKey] = true
		}
		if err := s.store.UpsertSession(existing); err != nil {
			return nil, err
		}
		bindSessionTranscript(p, existing, recordString(existing, "session_id"), threadID)
		return existing, nil
	}
	deliveryRoute := codexPreferredNativeDeliveryRoute(p)
	session := newSessionRecord(s.cfg.DeviceID, providerID, firstNonEmpty(stringAny(native["title"]), "Codex"), provider.StartOptions{Cwd: stringAny(native["cwd"])})
	session["session_id"] = logicalID
	session["native_session_id"] = threadID
	session["transcript_id"] = threadID
	session["delivery_route"] = "desktop_ipc"
	session["codex_control_route"] = deliveryRoute
	if hidden {
		session[hiddenFromSessionListsKey] = true
	}
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
	selectedCodexRoute := ""
	selectedClaudeRoute := ""
	existingClaudeStateChanged := false
	existing, found, findErr := s.findSessionForProviderAny(targetID, cliID)
	if findErr != nil {
		// Persisted ownership is authoritative. If it cannot be read, fail
		// closed instead of choosing the provider's current default route and
		// potentially introducing a competing owner for this native thread.
		writeError(w, http.StatusInternalServerError, findErr.Error())
		return
	}
	if found {
		if storedID := recordString(existing, "session_id"); storedID != "" {
			logicalID = storedID
		}
		if targetID == "codex" {
			selectedCodexRoute = codexControlRouteForProvider(target, existing)
			if selectedCodexRoute == "app_server" {
				selectedCodexRoute = codexAppServerDeliveryRoute(target)
			}
		} else if targetID == "claude" {
			existingClaudeStateChanged = normalizeClaudeControlCommitted(existing)
			selectedClaudeRoute = claudeControlRouteForBinding(target, existing, logicalID)
		}
	}
	if body.Fork {
		logicalID = newID()
	}
	if targetID == "codex" {
		if selectedCodexRoute == "" {
			selectedCodexRoute = codexPreferredNativeDeliveryRoute(target)
		}
		// Bind the exact route that will be persisted even for a new/legacy
		// record. Stale provider memory must not choose a different owner.
		bindSessionTranscript(target, state.Record{
			"provider_id":         "codex",
			"codex_control_route": selectedCodexRoute,
			"cwd":                 stringAny(native["cwd"]),
		}, logicalID, cliID)
	} else if targetID == "claude" {
		if selectedClaudeRoute == "" {
			selectedClaudeRoute = claudeControlRouteFromProvider(target, logicalID)
		}
		bindSessionTranscript(target, state.Record{
			"provider_id":             "claude",
			"claude_control_route":    selectedClaudeRoute,
			claudeControlCommittedKey: true,
			"cwd":                     stringAny(native["cwd"]),
		}, logicalID, cliID)
		// Persist the adopted owner before activation when a legacy logical
		// record already exists. This makes a restart during activation restore
		// the same route instead of selecting a new default.
		if found && canonicalClaudeControlRoute(recordString(existing, "claude_control_route")) == "" && selectedClaudeRoute != "" {
			existing["claude_control_route"] = selectedClaudeRoute
			existingClaudeStateChanged = true
		}
		if found && existingClaudeStateChanged {
			if err := s.store.UpsertSession(existing); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
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
		session["codex_control_route"] = selectedCodexRoute
		if selectedCodexRoute == "shared_daemon" || selectedCodexRoute == "desktop_ipc" {
			session["delivery_route"] = "desktop_ipc"
		} else {
			delete(session, "delivery_route")
		}
	} else if targetID == "claude" {
		// Resume always adopts an existing transcript and is therefore committed,
		// even if the selected owner is Desktop Computer Use.
		session[claudeControlCommittedKey] = true
		if !setClaudeControlRoute(session, target, logicalID) && selectedClaudeRoute != "" {
			session["claude_control_route"] = selectedClaudeRoute
		}
		bindSessionTranscript(target, session, logicalID, cliID)
	}
	session["state"] = "running"
	if targetID == "codex" {
		session["state"] = "idle"
	}
	if !body.Fork && (sessionHiddenFromLists(native, nil) ||
		sessionHiddenFromLists(native, s.hiddenSessionIDs(srcID))) {
		session[hiddenFromSessionListsKey] = true
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
	syncRefresh := queryBool(r.URL.Query().Get("sync")) || r.URL.Query().Get("include_stale") == "0"
	forceRefresh := queryBool(r.URL.Query().Get("refresh"))
	if syncRefresh {
		s.refreshNativeSessionCacheSync(providerID, p)
	} else if forceRefresh {
		// A user-initiated refresh is still stale-while-revalidate: start the
		// refresh immediately, but never hold the HTTP request on discovery.
		s.ensureNativeSessionRefresh(providerID, p, true)
	}
	sessions, meta := s.nativeSessionsForProvider(providerID, p, !forceRefresh)
	sessions = visibleSessionRows(sessions, s.hiddenSessionIDs(providerID))
	metaBody := map[string]any{
		"source":        meta.Source,
		"refreshed_at":  meta.RefreshedAt,
		"generation":    meta.Generation,
		"has_snapshot":  meta.HasSnapshot,
		"refreshing":    meta.Refreshing,
		"refresh_error": meta.Error,
	}
	deviceTime := currentDeviceTimeMetadata(s.cfg.DeviceID)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id":   providerID,
		"count":         len(sessions),
		"sessions":      sessions,
		"device_time":   deviceTime,
		"source":        meta.Source,
		"refreshed_at":  meta.RefreshedAt,
		"generation":    meta.Generation,
		"refreshing":    meta.Refreshing,
		"refresh_error": meta.Error,
		"meta":          metaBody,
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
	if !force && hadSnapshot && time.Since(entry.refreshedAt) < nativeSessionRefreshInterval(providerID) {
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

func nativeSessionRefreshInterval(providerID string) time.Duration {
	if canonicalProviderID(providerID) == "codex" {
		return nativeSessionRefreshCodex
	}
	return nativeSessionRefreshDefault
}

func (s *Server) refreshNativeSessionCacheSync(providerID string, p provider.Provider) {
	done, _ := s.ensureNativeSessionRefresh(providerID, p, true)
	if done != nil {
		<-done
	}
}

func (s *Server) refreshNativeSessionCache(providerID string, p provider.Provider, done chan struct{}) {
	rows, refreshErr := listNativeSessions(providerID, p)
	s.nativeMu.Lock()
	entry := s.nativeCache[providerID]
	if entry == nil {
		entry = &nativeSessionCacheEntry{}
		s.nativeCache[providerID] = entry
	}
	if refreshErr == nil {
		entry.sessions = rows
		entry.refreshedAt = time.Now()
		entry.generation++
		entry.err = ""
	} else {
		entry.err = refreshErr.Error()
	}
	entry.refreshing = false
	if entry.done == done {
		entry.done = nil
	}
	s.nativeMu.Unlock()
	close(done)
}

func listNativeSessions(providerID string, p provider.Provider) (rows []map[string]any, refreshErr error) {
	defer func() {
		if recover() != nil {
			rows = nil
			refreshErr = fmt.Errorf("panic while listing native sessions")
		}
	}()
	var raw []map[string]any
	if lister, ok := p.(nativeSessionErrorLister); ok {
		var err error
		raw, err = lister.ListNativeSessionsWithError()
		if err != nil {
			return nil, err
		}
	} else {
		raw = p.ListNativeSessions()
	}
	rows = cloneNativeSessionRows(providerID, raw)
	sortSessionRowsNewest(rows)
	return rows, nil
}

func (s *Server) nativeSessionCacheSnapshot(providerID string) ([]map[string]any, nativeSessionMeta) {
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	entry := s.nativeCache[providerID]
	if entry == nil {
		return nil, nativeSessionMeta{}
	}
	meta := nativeSessionMeta{
		Generation:  entry.generation,
		HasSnapshot: !entry.refreshedAt.IsZero(),
		Refreshing:  entry.refreshing,
		Error:       entry.err,
	}
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
	sortSessionRowsNewest(out)
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
		row := map[string]any{
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
		}
		if truthy(rec[hiddenFromSessionListsKey], false) {
			row[hiddenFromSessionListsKey] = true
		}
		rows = append(rows, row)
	}
	sortSessionRowsNewest(rows)
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
	for _, key := range []string{"session_id", "title", "cwd", "transcript_id", "native_session_id"} {
		if stringAny(row[key]) == "" && stringAny(stored[key]) != "" {
			row[key] = stored[key]
		}
	}
	mergeSessionActivity(row, stored)
	if truthy(stored["stored"], false) {
		row["stored"] = true
	}
	if truthy(stored[hiddenFromSessionListsKey], false) {
		row[hiddenFromSessionListsKey] = true
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
	if taskID == "" && sessionID == "" {
		hiddenSessions := s.hiddenStoredSessionKeys()
		filtered := make([]state.Record, 0, len(records))
		for _, rec := range records {
			if !taskHiddenFromLists(rec, hiddenSessions) {
				filtered = append(filtered, rec)
			}
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
		if status != "running" && status != "sent" && status != "attaching" && status != "waiting_approval" && status != "waiting_input" {
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
			updatedTask, _, _ := s.store.UpdateTask(recordString(rec, "task_id"), state.Record{"status": newStatus})
			s.publishTaskStream(providerID, sessionID, updatedTask)
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
	// An interrupt ends the authority to drive the desktop immediately. Do not
	// wait for an eventually delivered provider terminal frame: revoke the
	// lease before invoking the provider and synchronously require the helper to
	// confirm cleanup. The provider is still interrupted if relock confirmation
	// fails; its task must not keep running merely because cleanup reported a
	// fault, while the already-revoked lease remains fail-closed.
	cleanupErr := s.terminateComputerUseTarget(providerID, body.SessionID, "provider interrupt")
	res := p.Interrupt(body.SessionID)
	if cleanupErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "status": "error", "code": computerUseErrorCode(cleanupErr),
			"detail": "interrupt could not confirm computer-use relock: " + cleanupErr.Error(),
		})
		return
	}
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
	res := p.SetSessionModel(body.SessionID, body.Model, body.Effort)
	writeJSON(w, http.StatusOK, res)
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
		"decision": body.Decision, "confirmed": relay["confirmed"], "task": updated,
	})
}

type questionAnswerIn struct {
	ProviderID  string               `json:"provider_id"`
	SessionID   string               `json:"session_id"`
	RequestID   string               `json:"request_id"`
	Answers     map[string]string    `json:"answers"`
	AnswerItems []questionAnswerItem `json:"answer_items"`
}

type questionAnswerItem struct {
	Question string   `json:"question"`
	Selected []string `json:"selected"`
	Other    string   `json:"other"`
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
	if body.SessionID == "" || body.RequestID == "" || (len(body.Answers) == 0 && len(body.AnswerItems) == 0) {
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
	structured, flat, err := normalizedQuestionAnswers(body.Answers, body.AnswerItems)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var res map[string]any
	if answerer, ok := p.(provider.StructuredQuestionAnswerer); ok && len(body.AnswerItems) > 0 {
		res = answerer.AnswerQuestionStructured(body.SessionID, body.RequestID, structured)
	} else if answerer, ok := p.(interface {
		AnswerQuestion(sessionID string, requestID string, answers map[string]string) map[string]any
	}); ok {
		res = answerer.AnswerQuestion(body.SessionID, body.RequestID, flat)
	} else {
		writeError(w, http.StatusBadRequest, "provider does not support structured question answers")
		return
	}
	code := http.StatusOK
	if !truthy(res["ok"], false) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
}

func normalizedQuestionAnswers(
	legacy map[string]string, items []questionAnswerItem,
) (map[string]provider.QuestionAnswer, map[string]string, error) {
	structured := map[string]provider.QuestionAnswer{}
	flat := map[string]string{}
	if len(items) == 0 {
		for question, answer := range legacy {
			question = strings.TrimSpace(question)
			answer = strings.TrimSpace(answer)
			if question == "" || answer == "" {
				return nil, nil, errors.New("every question requires a non-empty answer")
			}
			flat[question] = answer
			structured[question] = provider.QuestionAnswer{Other: answer}
		}
		return structured, flat, nil
	}
	if len(items) > 32 {
		return nil, nil, errors.New("too many question answers")
	}
	for _, item := range items {
		question := strings.TrimSpace(item.Question)
		if question == "" || len(question) > 4096 {
			return nil, nil, errors.New("every answer requires a valid question")
		}
		if _, duplicate := structured[question]; duplicate {
			return nil, nil, errors.New("duplicate question answer")
		}
		seen := map[string]bool{}
		selected := make([]string, 0, len(item.Selected))
		for _, raw := range item.Selected {
			value := strings.TrimSpace(raw)
			if value == "" || len(value) > 4096 || seen[value] {
				continue
			}
			seen[value] = true
			selected = append(selected, value)
		}
		other := strings.TrimSpace(item.Other)
		if len(other) > 16*1024 {
			return nil, nil, errors.New("question answer is too long")
		}
		if len(selected) == 0 && other == "" {
			return nil, nil, errors.New("every question requires a non-empty answer")
		}
		structured[question] = provider.QuestionAnswer{Selected: selected, Other: other}
		values := append([]string(nil), selected...)
		if other != "" {
			values = append(values, other)
		}
		flat[question] = strings.Join(values, ", ")
	}
	return structured, flat, nil
}

func (s *Server) publishProviderStream(providerID string, target string, frame map[string]any) {
	if target == "" {
		return
	}
	if err := s.observeComputerUseProviderFrame(providerID, target, frame); err != nil {
		fmt.Fprintf(os.Stderr, "computer-use turn cleanup failed for %s/%s: %v\n", providerID, target, err)
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

// publishTaskStream emits only delivery metadata, never the prompt body. A
// Desktop-backed Codex tab may subscribe by either its logical session id or
// native transcript id, so fan the same lifecycle update out to both aliases.
func (s *Server) publishTaskStream(providerID string, sessionID string, task state.Record) {
	if task == nil {
		return
	}
	frame := map[string]any{
		"type": "task",
		"task": map[string]any{
			"task_id":        recordString(task, "task_id"),
			"session_id":     recordString(task, "session_id"),
			"provider_id":    canonicalProviderID(recordString(task, "provider_id")),
			"status":         recordString(task, "status"),
			"native_task_id": task["native_task_id"],
			"error":          recordString(task, "error"),
			"updated_at":     recordString(task, "updated_at"),
		},
	}
	targets := map[string]bool{sessionID: true}
	if session, ok, err := s.findSessionForProviderAny(providerID, sessionID); err == nil && ok {
		for _, field := range []string{"session_id", "native_session_id", "transcript_id"} {
			if target := recordString(session, field); target != "" {
				targets[target] = true
			}
		}
	}
	for target := range targets {
		s.publishProviderStream(providerID, target, frame)
	}
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
		previousActivity := map[string]any{
			"last_reply_at": stringAny(prev["last_reply_at"]),
			"updated_at":    stringAny(prev["updated_at"]),
		}
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
		}
		mergeSessionActivity(prev, previousActivity)
		mergeSessionActivity(prev, row)
		return true
	}
	for _, pid := range s.registry.IDs() {
		if filterProvider != "" && pid != filterProvider {
			continue
		}
		hiddenIDs := s.hiddenSessionIDs(pid)
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
				if sessionHiddenFromLists(row, hiddenIDs) || sessionHiddenFromLists(map[string]any(rec), hiddenIDs) {
					continue
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
			if sessionHiddenFromLists(native, hiddenIDs) {
				continue
			}
			transcript := firstNonEmpty(stringAny(native["cli_session_id"]), stringAny(native["native_session_id"]))
			if transcript == "" {
				continue
			}
			rec := byProviderTranscript[pid+":"+transcript]
			if sessionHiddenFromLists(map[string]any(rec), hiddenIDs) {
				continue
			}
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
	sortSessionRowsNewest(out)
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":    out,
		"device_time": currentDeviceTimeMetadata(s.cfg.DeviceID),
	})
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
	// Closing a logical session revokes desktop authority before either the
	// durable record or provider owner can disappear. The provider's eventual
	// terminal frame is not a cleanup guarantee: Locked Use must synchronously
	// confirm relock here, and a failure leaves the session available for retry.
	if err := s.terminateComputerUseTarget(providerID, logicalID, "provider session close"); err != nil {
		writeComputerUseError(w, fmt.Errorf("close session could not confirm computer-use relock: %w", err))
		return
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

func sortSessionRowsNewest(rows []map[string]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		left, leftOK := parseSessionTime(sessionSortAt(rows[i]))
		right, rightOK := parseSessionTime(sessionSortAt(rows[j]))
		switch {
		case leftOK && rightOK && !left.Equal(right):
			return left.After(right)
		case leftOK != rightOK:
			return leftOK
		}
		leftRaw, rightRaw := sessionSortAt(rows[i]), sessionSortAt(rows[j])
		if leftRaw != rightRaw {
			return leftRaw > rightRaw
		}
		return nativeSessionKey(rows[i]) < nativeSessionKey(rows[j])
	})
}

func parseSessionTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func newerSessionTime(left, right string) string {
	leftTime, leftOK := parseSessionTime(left)
	rightTime, rightOK := parseSessionTime(right)
	switch {
	case leftOK && rightOK:
		if rightTime.After(leftTime) {
			return right
		}
		return left
	case rightOK:
		return right
	case leftOK:
		return left
	case right > left:
		return right
	default:
		return left
	}
}

func mergeSessionActivity(target, source map[string]any) {
	target["last_reply_at"] = newerSessionTime(stringAny(target["last_reply_at"]), stringAny(source["last_reply_at"]))
	target["updated_at"] = newerSessionTime(stringAny(target["updated_at"]), stringAny(source["updated_at"]))
}

func currentDeviceTimeMetadata(deviceID string) map[string]any {
	now := time.Now()
	_, offsetSeconds := now.Zone()
	return map[string]any{
		"device_id":          deviceID,
		"time_zone":          localTimeZoneID(now.Location()),
		"utc_offset_minutes": offsetSeconds / 60,
		"observed_at":        now.UTC().Format(time.RFC3339Nano),
	}
}

func localTimeZoneID(location *time.Location) string {
	if location != nil {
		if name := usableTimeZoneID(location.String()); name != "" {
			return name
		}
	}
	if name := usableTimeZoneID(os.Getenv("TZ")); name != "" {
		return name
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if marker := strings.LastIndex(target, "/zoneinfo/"); marker >= 0 {
			if name := usableTimeZoneID(target[marker+len("/zoneinfo/"):]); name != "" {
				return name
			}
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := usableTimeZoneID(string(raw)); name != "" {
			return name
		}
	}
	return ""
}

func usableTimeZoneID(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), ":")
	if value == "" || value == "Local" || filepath.IsAbs(value) {
		return ""
	}
	if _, err := time.LoadLocation(value); err != nil {
		return ""
	}
	return value
}

func splitFields(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}
