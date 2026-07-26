package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

var codexThreadIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// codexPendingApproval is one unanswered human request (approval / user
// input) originating from either our own `codex app-server` child
// ("app_server") or a Desktop-owned turn mirrored over the Desktop IPC
// bridge ("desktop_ipc").
type codexPendingApproval struct {
	RequestID            any
	Key                  string
	RouteKey             string
	Method               string
	Params               map[string]any
	ThreadID             string
	TurnID               string
	ItemID               string
	ApprovalID           string
	Source               string
	CreatedAt            time.Time
	Instance             string
	ConnectionGeneration uint64
	Responding           bool
}

const (
	codexApprovalSourceAppServer = "app_server"
	codexApprovalSourceDesktop   = "desktop_ipc"
)

type Codex struct {
	id                      string
	cfg                     config.ProviderConfig
	command                 string
	cwd                     string
	approvalPolicy          string
	sandbox                 string
	nextModel               string
	nextEffort              string
	preferDesktopCodex      bool
	desktopSyncEnabled      bool
	desktopIPCSocket        string
	desktopIPCTimeout       time.Duration
	desktopAttachTimeout    time.Duration
	desktopIPCHostID        string
	nativeDeliveryRoute     string
	appServerTransport      string
	sharedDaemonCommand     string
	sharedDaemonAutostart   bool
	client                  codexAppClient
	clientMu                sync.Mutex
	connectMu               sync.Mutex
	clientGeneration        uint64
	clientSequence          uint64
	threadListMu            sync.Mutex
	threadListLegacy        bool
	discoveryMu             sync.Mutex
	discovery               *codexDiscoveryCatalog
	sessMu                  sync.Mutex
	threads                 map[string]string
	sessionStartOptions     map[string]map[string]any
	desktopSyncSessions     map[string]bool
	appServerResumeSessions map[string]bool
	sessionRoutes           map[string]string
	desktopOwnerClients     map[string]string
	approvalMu              sync.Mutex
	approvalsByThread       map[string][]*codexPendingApproval
	bridge                  *codexDesktopBridge
	bridgeMu                sync.Mutex
	desktopRefreshMu        sync.Mutex
	desktopRefreshAt        map[string]time.Time
	rateLimits              map[string]any
	rateLimitsByID          map[string]any
	streamPublisher         func(target string, frame map[string]any)
	runtimeMu               sync.Mutex
	activeThreads           map[string]time.Time
	appServerThreads        map[string]bool
	interruptingThreads     map[string]string
	pendingTools            map[string]map[string]map[string]any
	turnThreads             map[string]string
	planType                string
	lastState               string
	lastError               string
	lastChange              time.Time
	clientFactory           func(func(string, map[string]any), func(any, string, map[string]any)) codexAppClient
	desktopFactory          func() codexDesktopClient
	desktopOpener           func(string) error
}

func NewCodex(id string, cfg config.ProviderConfig) *Codex {
	c := &Codex{
		id:                      id,
		cfg:                     cfg,
		command:                 firstNonEmpty(cfg.Command, "codex"),
		cwd:                     expandUser(firstNonEmpty(cfg.Cwd, "~/Developer")),
		approvalPolicy:          stringExtra(cfg.Extra, "approval_policy", "never"),
		sandbox:                 stringExtra(cfg.Extra, "sandbox", "workspace-write"),
		nextModel:               stringExtra(cfg.Extra, "model", ""),
		nextEffort:              stringExtra(cfg.Extra, "effort", ""),
		preferDesktopCodex:      boolExtra(cfg.Extra, "prefer_desktop_codex", true),
		desktopSyncEnabled:      boolExtra(cfg.Extra, "desktop_sync", true),
		desktopIPCSocket:        stringExtra(cfg.Extra, "desktop_ipc_socket", ""),
		desktopIPCTimeout:       durationExtra(cfg.Extra, "desktop_ipc_timeout", 8*time.Second),
		desktopAttachTimeout:    durationExtra(cfg.Extra, "desktop_attach_timeout", 6*time.Second),
		desktopIPCHostID:        stringExtra(cfg.Extra, "desktop_ipc_host_id", "local"),
		nativeDeliveryRoute:     codexNativeDeliveryRoute(stringExtra(cfg.Extra, "native_delivery_route", "")),
		appServerTransport:      codexAppServerTransport(stringExtra(cfg.Extra, "app_server_transport", "")),
		sharedDaemonCommand:     stringExtra(cfg.Extra, "shared_daemon_command", ""),
		sharedDaemonAutostart:   boolExtra(cfg.Extra, "shared_daemon_autostart", false),
		threads:                 map[string]string{},
		sessionStartOptions:     map[string]map[string]any{},
		desktopSyncSessions:     map[string]bool{},
		appServerResumeSessions: map[string]bool{},
		sessionRoutes:           map[string]string{},
		desktopOwnerClients:     map[string]string{},
		approvalsByThread:       map[string][]*codexPendingApproval{},
		activeThreads:           map[string]time.Time{},
		appServerThreads:        map[string]bool{},
		interruptingThreads:     map[string]string{},
		pendingTools:            map[string]map[string]map[string]any{},
		turnThreads:             map[string]string{},
		desktopRefreshAt:        map[string]time.Time{},
		lastState:               "idle",
	}
	return c
}

func (c *Codex) SetStreamPublisher(publish func(target string, frame map[string]any)) {
	c.streamPublisher = publish
}

func (c *Codex) ID() string { return c.id }

// NativeDeliveryRoute tells the API how a read-only native Codex preview
// should be persisted before its first prompt. Current ChatGPT Desktop builds
// do not publish an owner on the VS Code private IPC router, while the
// supported shared app-server daemon can resume the exact persisted thread. Keep
// Desktop IPC available only as an explicit compatibility override.
func (c *Codex) NativeDeliveryRoute() string {
	if c.nativeDeliveryRoute == "desktop_ipc" && !c.desktopSyncEnabled {
		return "unavailable"
	}
	if c.appServerTransport != "shared_daemon" && c.nativeDeliveryRoute == "shared_daemon" {
		if c.desktopSyncEnabled {
			return "desktop_ipc"
		}
		return "unavailable"
	}
	return c.nativeDeliveryRoute
}

// AppServerDeliveryRoute identifies the concrete writer used for sessions
// created by remote-agent itself. Persisting this value keeps a restart or
// config change from silently moving an existing thread between the shared
// daemon and a legacy stdio child.
func (c *Codex) AppServerDeliveryRoute() string {
	return c.appServerTransport
}

func codexNativeDeliveryRoute(route string) string {
	if strings.EqualFold(strings.TrimSpace(route), "desktop_ipc") {
		return "desktop_ipc"
	}
	return "shared_daemon"
}

func codexAppServerTransport(transport string) string {
	if strings.EqualFold(strings.TrimSpace(transport), "stdio") {
		return "stdio"
	}
	return "shared_daemon"
}

// Installed reports whether the configured transport has its required Codex
// executable. Shared mode requires the official managed standalone layout;
// legacy stdio mode accepts the Desktop bundle or PATH.
func (c *Codex) Installed() bool {
	if c.appServerTransport == "shared_daemon" {
		_, err := c.resolveSharedDaemonCommand()
		return err == nil
	}
	return c.resolveCommand() != ""
}

func (c *Codex) Status() Status {
	cli := c.resolveCommand()
	if c.appServerTransport == "shared_daemon" {
		if managed, err := c.resolveSharedDaemonCommand(); err == nil {
			cli = managed
		} else {
			cli = ""
		}
	}
	err := (*string)(nil)
	if msg := c.getLastError(); msg != "" {
		err = &msg
	}
	return Status{
		ProviderID:  c.id,
		AppName:     firstNonEmpty(c.cfg.AppName, "Codex"),
		IsRunning:   cli != "",
		IsFrontmost: false,
		Installed:   cli != "",
		State:       c.getLastState(),
		LastError:   err,
		Capabilities: map[string]bool{
			"native_sessions": true, "native_task_status": true, "clipboard_output": false,
			"screenshot": false, "ocr": false, "approval": true, "app_server": true,
			"streaming": true, "steer": true, "interrupt": true, "rewind_user_message": true, "create_session": true,
		},
		Backend: map[bool]string{true: "codex_shared_app_server", false: "codex_app_server_go"}[c.appServerTransport == "shared_daemon"],
		Command: firstNonEmpty(cli, c.command),
		Cwd:     c.cwd,
		Account: c.accountBlock(),
	}
}

func (c *Codex) ModelSelect() ModelSelect {
	c.sessMu.Lock()
	nextModel, nextEffort := c.nextModel, c.nextEffort
	c.sessMu.Unlock()
	return ModelSelect{
		Models:        modelOptionsFromExtra(c.cfg.Extra["models"]),
		Efforts:       stringSliceExtra(c.cfg.Extra, "efforts", []string{"minimal", "low", "medium", "high"}),
		CurrentModel:  stringPtrIf(firstNonEmpty(c.currentClientModel(), nextModel)),
		CurrentEffort: stringPtrIf(nextEffort),
		Mode:          c.modeName(),
		Modes:         modeOptionsFromExtra(c.cfg.Extra["modes"], []ModeOption{{ID: "auto", Label: "Auto"}, {ID: "edit", Label: "Edit"}, {ID: "plan", Label: "Plan"}, {ID: "default", Label: "Ask"}}),
		Note:          "codex model is fixed when a thread is created; changes apply to the next session",
	}
}

func (c *Codex) ListNativeSessions() []map[string]any {
	var primary []map[string]any
	if client, err := c.ensureClient(); err == nil {
		if res, err := c.listCodexThreads(client); err == nil {
			primary = codexThreadListToSessions(res)
		}
	}
	localRows := c.localCodexSessions()
	if primary != nil {
		rows := mergeCodexNativeSessions(primary, localRows, nativeSessionListLimit)
		c.mergeRuntimeStatus(rows)
		return rows
	}
	return localRows
}

func mergeCodexNativeSessions(primary, local []map[string]any, limit int) []map[string]any {
	out := make([]map[string]any, 0, len(primary)+len(local))
	byID := map[string]map[string]any{}
	add := func(row map[string]any, preferExisting bool) {
		id := firstNonEmpty(stringAny(row["native_session_id"]), stringAny(row["cli_session_id"]))
		if id == "" {
			return
		}
		if existing := byID[id]; existing != nil {
			for key, value := range row {
				if !preferExisting || existing[key] == nil || stringAny(existing[key]) == "" {
					existing[key] = value
				}
			}
			for _, key := range []string{"updated_at", "last_reply_at"} {
				if newerCodexTimestamp(stringAny(existing[key]), stringAny(row[key])) == stringAny(row[key]) {
					existing[key] = row[key]
				}
			}
			return
		}
		cp := make(map[string]any, len(row))
		for key, value := range row {
			cp[key] = value
		}
		byID[id] = cp
		out = append(out, cp)
	}
	for _, row := range primary {
		add(row, false)
	}
	for _, row := range local {
		add(row, true)
	}
	sortByUpdated(out)
	return limitCodexSessionsByVisibility(out, limit)
}

func limitCodexSessionsByVisibility(rows []map[string]any, visibleLimit int) []map[string]any {
	if visibleLimit <= 0 {
		return rows
	}
	out := make([]map[string]any, 0, len(rows))
	visible := 0
	for _, row := range rows {
		if boolAny(row[hiddenFromSessionListsKey]) || boolAny(row["subagent"]) {
			// Hidden child threads stay in the provider's internal catalog so
			// exact-ID preview/resume/control remains possible, but they must
			// never consume the user-facing session budget.
			out = append(out, row)
			continue
		}
		if visible >= visibleLimit {
			continue
		}
		out = append(out, row)
		visible++
	}
	return out
}

func newerCodexTimestamp(left, right string) string {
	if right == "" {
		return left
	}
	if left == "" {
		return right
	}
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right)
	if leftErr == nil && rightErr == nil {
		if rightTime.After(leftTime) {
			return right
		}
		return left
	}
	if right > left {
		return right
	}
	return left
}

func (c *Codex) SessionMessages(sessionID string) ([]map[string]any, error) {
	threadID := c.threadForSession(sessionID)
	// Return the full logical history here; /session_preview owns tail/offset
	// pagination. Capping this at nativePreviewMaxItems turns the transcript
	// into a sliding window whose total stays constant while a turn is running.
	// The PWA then treats the moving indexes as stable and replaces earlier
	// messages with each newly appended item.
	if items := codexSessionMessages(threadID, stringSliceExtra(c.cfg.Extra, "codex_sessions_dirs", nil), nativePreviewUnlimited); len(items) > 0 {
		return items, nil
	}
	switch c.sessionRoute(sessionID) {
	case "desktop_ipc", "unavailable":
		// A read fallback must not create a competing app-server owner for a
		// Desktop-owned thread, nor bypass a fail-closed transport binding.
		return nil, nil
	}
	// Native previews are latency-sensitive and are polled by the PWA. The
	// app-server can take long enough to outlive the relay's upstream request,
	// leaving a real conversation rendered as "0 messages". Use it only as a
	// short fallback for a just-created thread whose rollout is not on disk yet.
	if client, err := c.ensureClient(); err == nil {
		if res, err := client.ThreadResume(threadID, nil, 2*time.Second); err == nil {
			if thread := codexThreadFromResume(res); thread != nil {
				if items := codexThreadToMessages(thread, nativePreviewUnlimited); len(items) > 0 {
					return items, nil
				}
			}
		}
	}
	return nil, nil
}

func (c *Codex) ReadSessionAsset(sessionID string, assetID string) (SessionAsset, bool, error) {
	return codexTranscriptAsset(c.threadForSession(sessionID), stringSliceExtra(c.cfg.Extra, "codex_sessions_dirs", nil), assetID)
}

func (c *Codex) SessionModel(sessionID string) map[string]any {
	st := c.SessionSettings(sessionID)
	out := map[string]any{}
	if v := stringAny(st["model"]); v != "" {
		out["model"] = v
	}
	if v := stringAny(st["effort"]); v != "" {
		out["effort"] = v
	}
	if v := stringAny(st["mode"]); v != "" {
		out["mode"] = v
	}
	threadID := c.boundThread(sessionID)
	if threadID == "" {
		threadID = sessionID
	}
	if usage := codexSessionUsage(threadID, stringSliceExtra(c.cfg.Extra, "codex_sessions_dirs", nil)); len(usage) > 0 {
		out["usage"] = usage
		if stringAny(out["model"]) == "" {
			out["model"] = stringAny(usage[len(usage)-1]["model"])
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *Codex) accountBlock() map[string]any {
	model := c.currentClientModel()
	c.sessMu.Lock()
	rl := c.rateLimits
	byID := c.rateLimitsByID
	planType := c.planType
	c.sessMu.Unlock()

	limits := []map[string]any{}
	for id, raw := range byID {
		snapshot := mapAny(raw)
		if len(snapshot) == 0 {
			continue
		}
		name := firstNonEmpty(stringAny(snapshot["limitName"]), id)
		limits = append(limits, map[string]any{
			"id": id, "name": name, "fast": codexFastLimit(name, id),
			"primary": codexRateWindow(snapshot["primary"]), "secondary": codexRateWindow(snapshot["secondary"]),
		})
	}
	sort.Slice(limits, func(i, j int) bool { return stringAny(limits[i]["name"]) < stringAny(limits[j]["name"]) })
	account := compactMap(map[string]any{
		"model": model, "plan_type": firstNonEmpty(planType, stringAny(rl["planType"])),
		"fast_mode": codexFastLimit(model, stringAny(rl["limitName"]), stringAny(rl["limitId"])),
		"primary":   codexRateWindow(rl["primary"]), "secondary": codexRateWindow(rl["secondary"]),
		"rate_limit_reached": firstValue(rl["rateLimitReachedType"], rl["rate_limit_reached"]),
	})
	if len(limits) > 0 {
		account["limits"] = limits
	}
	if len(account) == 0 {
		return nil
	}
	return account
}

func codexRateWindow(raw any) map[string]any {
	w := mapAny(raw)
	if len(w) == 0 {
		return nil
	}
	return compactMap(map[string]any{
		"used_percent": firstValue(w["usedPercent"], w["used_percent"]),
		"window_mins":  firstValue(firstValue(w["windowDurationMins"], w["window_minutes"]), w["window_mins"]),
		"resets_at":    firstValue(w["resetsAt"], w["resets_at"]),
	})
}

func codexFastLimit(parts ...string) bool {
	for _, part := range parts {
		v := strings.ToLower(part)
		if strings.Contains(v, "spark") || strings.Contains(v, "bengalfox") || strings.Contains(v, "fast") {
			return true
		}
	}
	return false
}

func (c *Codex) ReferencedFiles(sessionID string) map[string]bool {
	msgs, _ := c.SessionMessages(sessionID)
	return referencedFilesFromMessages(msgs)
}

func (c *Codex) OpenOrCreateSession(sessionID string, opts StartOptions) (string, error) {
	client, err := c.ensureClient()
	if err != nil {
		c.setLastError(err.Error())
		c.setLastState("error")
		return "", err
	}
	startOpts := c.threadStartOptions(opts.Cwd, opts.Model, opts.Effort, opts.Mode)
	tid, err := client.ThreadStart(startOpts)
	if err != nil {
		c.setLastError(err.Error())
		c.setLastState("error")
		return "", err
	}
	c.BindSessionRoute(sessionID, tid, c.appServerTransport, opts.Cwd)
	c.markAppServerSessionReady(sessionID)
	c.markAppServerThread(tid)
	c.setStartOptions(sessionID, startOpts)
	c.setLastError("")
	c.setLastState("idle")
	return tid, nil
}

func (c *Codex) OpenResumeSession(sessionID string, resumeID string, cwd string, fork bool) (string, error) {
	route := c.sessionRoute(sessionID)
	if route == "" {
		route = c.NativeDeliveryRoute()
	}
	if route == "desktop_ipc" {
		if fork {
			err := errors.New("native Codex fork is unsupported over Desktop IPC")
			c.setLastError(err.Error())
			return "", err
		}
		c.BindSessionRoute(sessionID, resumeID, route, firstNonEmpty(cwd, c.cwd))
		if err := c.attachDesktopOwner(resumeID); err != nil {
			c.setLastError(err.Error())
			c.setLastState("error")
			return "", err
		}
		c.refreshDesktopPendingState(resumeID)
		c.setLastError("")
		c.setLastState("idle")
		return resumeID, nil
	}
	if route != "shared_daemon" && route != "stdio" {
		err := fmt.Errorf("native Codex resume/fork route is unavailable: %s", route)
		c.setLastError(err.Error())
		return "", err
	}
	if route != c.appServerTransport {
		err := fmt.Errorf("persisted Codex owner route %s is unavailable", route)
		c.setLastError(err.Error())
		return "", err
	}
	client, err := c.ensureClient()
	if err != nil {
		c.setLastError(err.Error())
		return "", err
	}
	var result any
	if fork {
		result, err = client.ThreadFork(resumeID, nil)
		if err != nil && isThreadNotFound(err) {
			if _, resumeErr := c.resumeThread(client, resumeID, resumeID, cwd); resumeErr != nil {
				c.setLastError(resumeErr.Error())
				return "", resumeErr
			}
			result, err = client.ThreadFork(resumeID, nil)
		}
	} else {
		var tid string
		tid, err = c.resumeThread(client, sessionID, resumeID, cwd)
		result = map[string]any{"thread": map[string]any{"id": tid}}
	}
	if err != nil {
		c.setLastError(err.Error())
		return "", err
	}
	tid := stringAny(mapAny(mapAny(result)["thread"])["id"])
	if tid == "" {
		err := errors.New("native Codex resume/fork returned no thread id")
		c.setLastError(err.Error())
		return "", err
	}
	if fork && tid == resumeID {
		err := errors.New("native Codex fork returned the source thread id")
		c.setLastError(err.Error())
		return "", err
	}
	c.BindSessionRoute(sessionID, tid, route, firstNonEmpty(cwd, c.cwd))
	c.markAppServerSessionReady(sessionID)
	c.markAppServerThread(tid)
	c.setLastError("")
	c.setLastState("idle")
	return tid, nil
}

func (c *Codex) RewindUserMessage(opts RewindUserMessageOptions) (RewindUserMessageResult, error) {
	opts.SessionID = strings.TrimSpace(opts.SessionID)
	opts.ThreadID = strings.TrimSpace(opts.ThreadID)
	opts.TurnID = strings.TrimSpace(opts.TurnID)
	if opts.SessionID == "" {
		return RewindUserMessageResult{}, errors.New("session_id is required")
	}
	if opts.ThreadID == "" {
		return RewindUserMessageResult{}, errors.New("thread_id is required")
	}
	if opts.TurnID == "" {
		return RewindUserMessageResult{}, errors.New("turn_id is required")
	}
	if strings.TrimSpace(opts.Prompt) == "" {
		return RewindUserMessageResult{}, errors.New("prompt is empty")
	}
	threadID := c.threadForSession(opts.ThreadID)
	if threadID == "" {
		return RewindUserMessageResult{}, errors.New("codex rewind source has no native thread")
	}
	sourceRoute := c.sessionRoute(opts.ThreadID)
	if sourceRoute == "" && threadID == opts.ThreadID && codexThreadIDRE.MatchString(threadID) {
		sourceRoute = c.NativeDeliveryRoute()
	}
	if sourceRoute != "shared_daemon" && sourceRoute != "stdio" {
		err := errors.New("codex rewind requires an app-server-owned source session")
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	if sourceRoute != c.appServerTransport {
		err := fmt.Errorf("codex rewind owner transport %s is unavailable", sourceRoute)
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	client, err := c.ensureClient()
	if err != nil {
		c.setLastError(err.Error())
		c.setLastState("error")
		return RewindUserMessageResult{}, err
	}
	if active := c.SessionRunning(opts.ThreadID); active != nil && *active {
		err := errors.New("a turn is already in progress for this codex thread")
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	res, err := client.ThreadResume(threadID, nil, c.appServerResumeTimeout())
	if err != nil {
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	thread := codexThreadFromResume(res)
	if thread == nil {
		err := errors.New("codex thread resume returned no thread")
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	resumedID := firstNonEmpty(stringAny(thread["id"]), stringAny(thread["threadId"]))
	if resumedID != threadID {
		err := fmt.Errorf("thread/resume route mismatch: requested %s, received %s", threadID, resumedID)
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	targetCwd := firstNonEmpty(opts.Cwd, c.cwd)
	returnedCwd := firstNonEmpty(stringAny(thread["cwd"]), stringAny(mapAny(res)["cwd"]))
	if returnedCwd != "" && !sameCodexCwd(returnedCwd, targetCwd) {
		err := fmt.Errorf(
			"thread/resume cwd mismatch for %s: requested %q, received %q",
			threadID,
			targetCwd,
			returnedCwd,
		)
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	status := firstNonEmpty(
		stringAny(mapAny(thread["status"])["type"]),
		stringAny(thread["status"]),
	)
	if status == "" {
		status = "idle"
	}
	activeTurnID, activeTurns := codexResumedActiveTurn(thread)
	if activeTurns > 1 || (status == "active" && activeTurns != 1) ||
		(status != "active" && activeTurns != 0) {
		err := fmt.Errorf("thread/resume returned inconsistent active turn state for %s", threadID)
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	client.SetThreadStatus(threadID, status)
	c.setThreadActive(threadID, status == "active")
	if activeTurnID != "" {
		client.SetThreadTurn(threadID, activeTurnID)
		c.bindTurnThread(threadID, activeTurnID)
	}
	if status == "active" {
		err := errors.New("a turn is already in progress for this codex thread")
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	numTurns, err := codexRollbackTurnCount(thread, opts.TurnID)
	if err != nil {
		c.setLastError(err.Error())
		return RewindUserMessageResult{}, err
	}
	if _, err := client.ThreadRollback(threadID, numTurns, nil); err != nil {
		c.setLastError(err.Error())
		c.setLastState("error")
		return RewindUserMessageResult{}, err
	}
	startOpts := c.startOptionsFor(opts.SessionID)
	if startOpts == nil {
		startOpts = map[string]any{}
	}
	if opts.Cwd != "" {
		startOpts["cwd"] = opts.Cwd
	}
	c.setStartOptions(opts.SessionID, startOpts)
	c.BindSessionRoute(opts.SessionID, threadID, sourceRoute, targetCwd)
	c.markAppServerSessionReady(opts.SessionID)
	c.markAppServerThread(threadID)
	turnID, route, err := c.startTurnWithBudget(
		opts.SessionID,
		threadID,
		opts.Prompt,
		nil,
		codexAppServerMutationTimeout,
		false,
	)
	if err != nil {
		c.setLastError(err.Error())
		c.setLastState("error")
		return RewindUserMessageResult{}, err
	}
	if turnID != "" {
		client.SetThreadTurn(threadID, turnID)
	}
	if route != "Codex app-server" {
		c.setThreadActive(threadID, true)
		c.setLastError("")
		c.setLastState("running")
	}
	return RewindUserMessageResult{
		SessionID:    opts.SessionID,
		ThreadID:     threadID,
		TurnID:       turnID,
		State:        "running",
		Message:      "codex thread rewound and edited turn started via " + route,
		NativeTaskID: threadID,
	}, nil
}

func (c *Codex) CloseSession(sessionID string) map[string]any {
	c.sessMu.Lock()
	tid := c.threads[sessionID]
	delete(c.threads, sessionID)
	delete(c.sessionStartOptions, sessionID)
	delete(c.desktopSyncSessions, sessionID)
	delete(c.appServerResumeSessions, sessionID)
	delete(c.sessionRoutes, sessionID)
	c.sessMu.Unlock()
	c.clearDesktopOwnerClient(tid)
	return map[string]any{"ok": true, "killed": false, "detail": "thread detached (persists on disk; attach by id)"}
}

func (c *Codex) BindTranscript(sessionID string, transcriptID string) {
	if sessionID == "" || transcriptID == "" {
		return
	}
	c.bindThread(sessionID, transcriptID)
}

// BindSessionRoute restores the persisted mutable owner route and cwd. Plain
// BindTranscript remains a read-side alias and must not change ownership.
func (c *Codex) BindSessionRoute(sessionID string, transcriptID string, route string, cwd string) {
	if sessionID == "" || transcriptID == "" {
		return
	}
	route = strings.TrimSpace(route)
	if route == "app_server" {
		route = c.appServerTransport
	}
	if route == "shared_daemon" && c.appServerTransport != "shared_daemon" {
		// A persisted owner route is authoritative. Configuration drift must
		// fail closed instead of moving a thread to a different writer.
		route = "unavailable"
	}
	if route == "stdio" && c.appServerTransport != "stdio" {
		route = "unavailable"
	}
	if route == "desktop_ipc" && !c.desktopSyncEnabled {
		route = "unavailable"
	}
	if route != "desktop_ipc" && route != "shared_daemon" && route != "stdio" && route != "unavailable" {
		route = "unavailable"
	}
	c.sessMu.Lock()
	alreadyReady := c.threads[sessionID] == transcriptID &&
		c.sessionRoutes[sessionID] == route &&
		!c.appServerResumeSessions[sessionID]
	c.threads[sessionID] = transcriptID
	c.sessionRoutes[sessionID] = route
	if cwd != "" {
		opts := c.sessionStartOptions[sessionID]
		if opts == nil {
			opts = map[string]any{}
		}
		opts["cwd"] = expandUser(cwd)
		c.sessionStartOptions[sessionID] = opts
	}
	if route == "desktop_ipc" {
		c.desktopSyncSessions[sessionID] = true
		delete(c.appServerResumeSessions, sessionID)
	} else if route == "shared_daemon" || route == "stdio" {
		delete(c.desktopSyncSessions, sessionID)
		if !alreadyReady {
			c.appServerResumeSessions[sessionID] = true
		}
	} else {
		delete(c.desktopSyncSessions, sessionID)
		delete(c.appServerResumeSessions, sessionID)
	}
	c.sessMu.Unlock()
	if route == "desktop_ipc" {
		c.markDesktopOwnedThread(transcriptID)
	} else {
		c.clearDesktopOwnerClient(transcriptID)
	}
	if route == "desktop_ipc" {
		c.scheduleDesktopPendingRefresh(transcriptID)
	}
}

// BindDesktopTranscript is the compatibility binder for providers/API
// versions that do not yet pass an explicit route.
func (c *Codex) BindDesktopTranscript(sessionID string, transcriptID string) {
	c.BindSessionRoute(sessionID, transcriptID, "desktop_ipc", "")
}

func (c *Codex) SendPrompt(sessionID string, prompt string) SendResult {
	return c.SendPromptWithAttachments(sessionID, prompt, nil)
}

func (c *Codex) SendPromptWithAttachments(sessionID string, prompt string, attachments []Attachment) SendResult {
	if strings.TrimSpace(prompt) == "" && len(attachments) == 0 {
		msg := "empty prompt"
		return SendResult{OK: false, State: c.getLastState(), Error: &msg}
	}
	tid, err := c.threadForDelivery(sessionID)
	if err != nil {
		c.setLastError(err.Error())
		msg := err.Error()
		return SendResult{OK: false, State: "error", Error: &msg}
	}
	if active := c.SessionRunning(sessionID); active != nil && *active {
		msg := "a turn is already in progress for this session"
		return SendResult{OK: false, State: firstNonEmpty(c.getLastState(), "running"), Error: &msg}
	}
	if turnID, route, err := c.startTurnWithAttachments(sessionID, tid, prompt, attachments); err != nil {
		c.setLastError(err.Error())
		msg := err.Error()
		return SendResult{OK: false, State: "error", Error: &msg}
	} else {
		c.clientMu.Lock()
		client := c.client
		c.clientMu.Unlock()
		if turnID != "" {
			if client != nil {
				client.SetThreadTurn(tid, turnID)
			}
		}
		if route != "Codex app-server" {
			c.setThreadActive(tid, true)
			c.setLastError("")
			c.setLastState("running")
		}
		return SendResult{OK: true, State: "running", Message: "turn started via " + route, NativeTaskID: tid}
	}
}

func (c *Codex) LatestOutput(sessionID string) map[string]any {
	msgs, _ := c.SessionMessages(sessionID)
	text := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if s := stringAny(msgs[i]["text"]); s != "" {
			text = s
			break
		}
	}
	_, pending := c.pendingApprovalsForSession(sessionID)
	return map[string]any{"source": "codex_app_server", "text": text, "approval_required": len(pending) > 0}
}

// DetectState is session-scoped: one thread going idle must not hide another
// thread's pending approval, and vice versa.
func (c *Codex) DetectState(sessionID string) string {
	if sessionID != "" {
		if _, pending := c.pendingApprovalsForSession(sessionID); len(pending) > 0 {
			return "waiting_approval"
		}
		if active := c.SessionRunning(sessionID); active != nil && *active {
			return "running"
		}
		return "idle"
	}
	if c.anyAppServerApprovals() {
		return "waiting_approval"
	}
	return c.getLastState()
}

// RelayApproval answers the most recent pending approval for the session.
// Prefer RelayApprovalRequest with an explicit request id; this remains for
// callers without one.
func (c *Codex) RelayApproval(sessionID string, decision string) map[string]any {
	_, pending := c.pendingApprovalsForSession(sessionID)
	if len(pending) == 0 {
		return map[string]any{"ok": false, "status": "stale", "detail": "no pending codex approval for this session"}
	}
	return c.relayApprovalDecision(sessionID, pending[len(pending)-1], decision)
}

// RelayApprovalRequest answers one specific pending approval, addressed by
// its stable request id. A response that arrives after the request was
// already resolved (Desktop answered first, turn ended, auto_review handled
// it) reports status "stale" instead of touching another request.
func (c *Codex) RelayApprovalRequest(sessionID string, requestID string, decision string) map[string]any {
	if strings.TrimSpace(requestID) == "" {
		return c.RelayApproval(sessionID, decision)
	}
	_, pending := c.pendingApprovalsForSession(sessionID)
	match, ambiguous := codexPendingByDisplayID(pending, requestID)
	if ambiguous {
		return map[string]any{
			"ok": false, "status": "ambiguous", "request_id": requestID,
			"detail": "multiple typed Codex requests share this display id; refresh before responding",
		}
	}
	if match != nil {
		return c.relayApprovalDecision(sessionID, match, decision)
	}
	return map[string]any{"ok": false, "status": "stale", "request_id": requestID,
		"detail": "approval request not found (already resolved elsewhere?)"}
}

func (c *Codex) relayApprovalDecision(sessionID string, req *codexPendingApproval, decision string) map[string]any {
	if decision != "allow" && decision != "deny" {
		return map[string]any{"ok": false, "detail": "decision must be allow or deny"}
	}
	if req.Method == "item/tool/requestUserInput" {
		return map[string]any{"ok": false, "detail": "this request needs structured answers; use /question_answer", "request_id": req.Key}
	}
	var err error
	switch req.Source {
	case codexApprovalSourceDesktop:
		err = c.relayDesktopApproval(req, decision)
	default:
		err = c.relayAppServerApproval(req, decision)
	}
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error(), "request_id": req.Key}
	}
	c.settleAfterApproval(sessionID)
	status := "relayed"
	confirmed := true
	if req.Source == codexApprovalSourceAppServer {
		// A successful response write is only transport acceptance. Keep the
		// immutable request route until serverRequest/resolved or turn terminal.
		status = "responding"
		confirmed = false
	}
	return map[string]any{
		"ok": true, "status": status, "confirmed": confirmed,
		"decision": decision, "method": req.Method, "request_id": req.Key,
	}
}

func (c *Codex) relayAppServerApproval(req *codexPendingApproval, decision string) error {
	body, err := codexApprovalResponseBody(req.Method, decision == "allow", req.Params)
	if err != nil {
		return err
	}
	return c.relayAppServerResponse(req, body)
}

func (c *Codex) relayAppServerResponse(req *codexPendingApproval, body map[string]any) error {
	client, err := c.beginAppServerResponse(req)
	if err != nil {
		return err
	}
	if err := client.Respond(req.RequestID, body); err != nil {
		c.rollbackAppServerResponse(req)
		return err
	}
	c.publishApprovalChanged(req.ThreadID)
	return nil
}

func (c *Codex) beginAppServerResponse(req *codexPendingApproval) (codexAppClient, error) {
	if req == nil || req.Source != codexApprovalSourceAppServer {
		return nil, errors.New("invalid Codex app-server request route")
	}
	if c.threadInterrupting(req.ThreadID) {
		return nil, errors.New("Codex turn cancellation is already requested")
	}
	client, generation := c.currentClientRoute()
	if client == nil || generation == 0 || req.ConnectionGeneration != generation {
		return nil, errors.New("Codex approval belongs to a stale app-server connection")
	}
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	for _, candidate := range c.approvalsByThread[req.ThreadID] {
		if candidate.Instance != req.Instance || candidate.RouteKey != req.RouteKey {
			continue
		}
		if candidate.Responding {
			return nil, errors.New("Codex approval response is already awaiting source confirmation")
		}
		candidate.Responding = true
		return client, nil
	}
	return nil, errors.New("Codex approval request is no longer pending")
}

func (c *Codex) rollbackAppServerResponse(req *codexPendingApproval) {
	if req == nil {
		return
	}
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	for _, candidate := range c.approvalsByThread[req.ThreadID] {
		if candidate.Instance == req.Instance && candidate.RouteKey == req.RouteKey {
			candidate.Responding = false
			return
		}
	}
}

func (c *Codex) relayDesktopApproval(req *codexPendingApproval, decision string) error {
	responder, ok := c.desktopClient().(codexDesktopApprovalResponder)
	if !ok {
		return errors.New("desktop IPC client does not support approval responses")
	}
	owner := c.desktopOwnerClient(req.ThreadID)
	if owner == "" {
		if b := c.desktopBridge(); b != nil {
			owner = b.OwnerClient(req.ThreadID)
		}
	}
	timeout := c.desktopIPCTimeout
	switch req.Method {
	case "item/commandExecution/requestApproval":
		value, err := codexCommandDecisionValue(decision == "allow", req.Params)
		if err != nil {
			return err
		}
		_, err = responder.CommandApprovalDecision(req.ThreadID, req.RequestID, value, owner, timeout)
		return err
	case "item/fileChange/requestApproval":
		value, err := codexCommandDecisionValue(decision == "allow", req.Params)
		if err != nil {
			return err
		}
		_, err = responder.FileApprovalDecision(req.ThreadID, req.RequestID, value, owner, timeout)
		return err
	case "item/permissions/requestApproval":
		body, err := codexApprovalResponseBody(req.Method, decision == "allow", req.Params)
		if err != nil {
			return err
		}
		_, err = responder.PermissionsApprovalResponse(req.ThreadID, req.RequestID, body, owner, timeout)
		return err
	case "mcpServer/elicitation/request":
		body, err := codexApprovalResponseBody(req.Method, decision == "allow", req.Params)
		if err != nil {
			return err
		}
		_, err = responder.SubmitMcpElicitationResponse(req.ThreadID, req.RequestID, body, owner, timeout)
		return err
	}
	return errors.New("unsupported desktop approval method: " + req.Method)
}

func (c *Codex) settleAfterApproval(sessionID string) {
	_, remaining := c.pendingApprovalsForSession(sessionID)
	if len(remaining) > 0 {
		c.setLastState("waiting_approval")
	} else {
		c.setLastState("running")
	}
}

// AnswerQuestion submits structured answers for a pending
// item/tool/requestUserInput. Answers may be keyed by question id, question
// text, or zero-based index.
func (c *Codex) AnswerQuestion(sessionID string, requestID string, answers map[string]string) map[string]any {
	_, pending := c.pendingApprovalsForSession(sessionID)
	req, ambiguous := codexPendingByDisplayID(pending, requestID)
	if ambiguous {
		return map[string]any{
			"ok": false, "status": "ambiguous", "request_id": requestID,
			"detail": "multiple typed Codex requests share this display id; refresh before responding",
		}
	}
	if req == nil {
		return map[string]any{"ok": false, "status": "stale", "detail": "question request not found (already resolved?)", "request_id": requestID}
	}
	if req.Method != "item/tool/requestUserInput" {
		return map[string]any{"ok": false, "detail": "request is not a user-input question", "request_id": requestID}
	}
	body, err := codexUserInputResponseBody(req.Params, answers)
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error(), "request_id": requestID}
	}
	if req.Source == codexApprovalSourceDesktop {
		responder, ok := c.desktopClient().(codexDesktopApprovalResponder)
		if !ok {
			return map[string]any{"ok": false, "detail": "desktop IPC client does not support user input responses"}
		}
		owner := c.desktopOwnerClient(req.ThreadID)
		if owner == "" {
			if b := c.desktopBridge(); b != nil {
				owner = b.OwnerClient(req.ThreadID)
			}
		}
		if _, err := responder.SubmitUserInput(req.ThreadID, req.RequestID, body, owner, c.desktopIPCTimeout); err != nil {
			return map[string]any{"ok": false, "detail": err.Error(), "request_id": requestID}
		}
	} else {
		if err := c.relayAppServerResponse(req, body); err != nil {
			return map[string]any{"ok": false, "detail": err.Error(), "request_id": requestID}
		}
	}
	c.settleAfterApproval(sessionID)
	status := "relayed"
	confirmed := true
	if req.Source == codexApprovalSourceAppServer {
		status = "responding"
		confirmed = false
	}
	return map[string]any{"ok": true, "status": status, "confirmed": confirmed, "request_id": requestID}
}

// ApprovalRequest describes the oldest pending approval for the session; the
// web client responds via /approval with the embedded request_id.
func (c *Codex) ApprovalRequest(sessionID string) map[string]any {
	_, pending := c.pendingApprovalsForSession(sessionID)
	if len(pending) == 0 {
		return nil
	}
	req := pending[0]
	out := codexApprovalView(req)
	out["pending_count"] = len(pending)
	return out
}

func (c *Codex) SendKeys(sessionID string, keys []string) map[string]any {
	return map[string]any{"ok": false, "detail": "codex app-server has no raw key relay"}
}

func (c *Codex) Interrupt(sessionID string) map[string]any {
	tid, err := c.threadForDelivery(sessionID)
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	if c.threadInterrupting(tid) {
		return map[string]any{
			"ok": true, "status": "cancellation_requested", "confirmed": false,
			"detail": "turn cancellation is already pending authoritative Codex completion",
		}
	}
	// Register cancellation before issuing the RPC. turn/completed can race
	// ahead of the RPC response; a terminal notification must be able to retire
	// this marker and must never be overwritten by a later transport ACK.
	c.markThreadInterrupting(tid, "")
	route := "Codex app-server"
	if c.shouldTryDesktopSync(sessionID, tid) {
		if c.ensureDesktopOwnerClient(tid) == "" {
			if err = c.attachDesktopOwner(tid); err != nil {
				c.rollbackThreadInterrupting(tid)
				return map[string]any{"ok": false, "detail": err.Error()}
			}
		}
		err = c.tryDesktopInterruptTurn(sessionID, tid)
		if err == nil {
			route = "Codex Desktop"
		} else if isNoDesktopOwnerClient(err) {
			// no-client-found proves the previous targeted request was not
			// delivered. Reopen the Desktop thread and retry once on its
			// newly registered owner; never cross the ownership boundary by
			// falling back to app-server.
			if err = c.attachDesktopOwner(tid); err == nil {
				err = c.tryDesktopInterruptTurn(sessionID, tid)
			}
			if err == nil {
				route = "Codex Desktop"
			}
		}
		if err != nil {
			c.rollbackThreadInterrupting(tid)
			return map[string]any{"ok": false, "detail": err.Error()}
		}
	}
	if route == "Codex app-server" {
		client, clientErr := c.ensureClient()
		if clientErr != nil {
			c.rollbackThreadInterrupting(tid)
			return map[string]any{"ok": false, "detail": clientErr.Error()}
		}
		if _, err = client.TurnInterrupt(tid, nil); err != nil {
			c.rollbackThreadInterrupting(tid)
			return map[string]any{"ok": false, "detail": err.Error()}
		}
	}
	// turn/interrupt and Desktop IPC responses only acknowledge delivery of the
	// cancellation request. Keep the thread live until the owning Codex
	// runtime publishes its authoritative terminal state.
	c.publishApprovalChanged(tid)
	if !c.threadInterrupting(tid) {
		return map[string]any{
			"ok": true, "status": "completed", "confirmed": true,
			"detail": "Codex reached an authoritative terminal state while cancellation was acknowledged",
		}
	}
	c.setLastState("running")
	return map[string]any{
		"ok": true, "status": "cancellation_requested", "confirmed": false,
		"detail": "turn cancellation requested via " + route + "; waiting for Codex completion",
	}
}

func (c *Codex) Steer(sessionID string, prompt string) map[string]any {
	if strings.TrimSpace(prompt) == "" {
		return map[string]any{"ok": false, "detail": "empty prompt"}
	}
	tid, err := c.threadForDelivery(sessionID)
	if err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	route := "Codex app-server"
	if c.shouldTryDesktopSync(sessionID, tid) {
		if c.ensureDesktopOwnerClient(tid) == "" {
			if err = c.attachDesktopOwner(tid); err != nil {
				return map[string]any{"ok": false, "detail": err.Error()}
			}
		}
		err = c.tryDesktopSteerTurn(sessionID, tid, prompt)
		if err == nil {
			route = "Codex Desktop"
		} else if isNoDesktopOwnerClient(err) {
			// As with start/interrupt, a Desktop-bound session remains on
			// its persisted delivery route even while the renderer is
			// reattaching.
			if err = c.attachDesktopOwner(tid); err == nil {
				err = c.tryDesktopSteerTurn(sessionID, tid, prompt)
			}
			if err == nil {
				route = "Codex Desktop"
			}
		}
		if err != nil {
			return map[string]any{"ok": false, "detail": err.Error()}
		}
	}
	if route == "Codex app-server" {
		client, clientErr := c.ensureClient()
		if clientErr != nil {
			return map[string]any{"ok": false, "detail": clientErr.Error()}
		}
		if _, err = client.TurnSteer(tid, prompt, nil); err != nil {
			return map[string]any{"ok": false, "detail": err.Error()}
		}
	}
	return map[string]any{"ok": true, "detail": "steered into running turn via " + route}
}

func (c *Codex) SetSessionModel(sessionID string, model string, effort string) map[string]any {
	c.sessMu.Lock()
	if model != "" {
		c.nextModel = model
	}
	if effort != "" {
		c.nextEffort = effort
	}
	c.sessMu.Unlock()
	return map[string]any{"ok": true, "applied": "next_session"}
}

func (c *Codex) SessionRunning(sessionID string) *bool {
	c.clientMu.Lock()
	client := c.client
	c.clientMu.Unlock()
	tid := c.threadForSession(sessionID)
	if tid == "" {
		return nil
	}
	switch c.sessionRoute(sessionID) {
	case "unavailable":
		return nil
	case "desktop_ipc":
		return c.desktopThreadRunning(tid)
	}
	if client == nil {
		if c.threadActive(tid) {
			v := true
			return &v
		}
		return nil
	}
	if _, ok := client.ThreadStatus(tid); ok {
		if client.IsActive(tid) {
			v := true
			c.setThreadActive(tid, true)
			return &v
		}
	}
	if c.threadActive(tid) {
		v := true
		return &v
	}
	return nil
}

func (c *Codex) desktopThreadRunning(threadID string) *bool {
	rows := c.desktopRuntimeSessions()
	if rows == nil {
		return nil
	}
	for _, row := range rows {
		tid := firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["native_session_id"]))
		if tid != threadID {
			continue
		}
		live := row["live"] == nil || boolAny(row["live"])
		status := stringAny(row["status"])
		running := live && (status == "" || status == "active" || status == "running" || status == "inProgress")
		c.setThreadActive(threadID, running)
		return &running
	}
	c.setThreadActive(threadID, false)
	v := false
	return &v
}

func (c *Codex) RuntimeSessions() []map[string]any {
	now := time.Now()
	seen := map[string]bool{}
	rows := []map[string]any{}
	appendRow := func(row map[string]any) {
		tid := firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["native_session_id"]))
		if tid == "" || seen[tid] {
			return
		}
		seen[tid] = true
		if stringAny(row["session_id"]) == "" {
			row["session_id"] = tid
		}
		row["provider_id"] = c.id
		if stringAny(row["updated_at"]) == "" {
			row["updated_at"] = now.UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	appendThread := func(tid string, at time.Time) {
		if tid == "" || seen[tid] {
			return
		}
		if at.IsZero() {
			at = now
		}
		sid := c.sessionForThread(tid)
		if sid == "" {
			sid = tid
		}
		appendRow(map[string]any{
			"session_id":        sid,
			"provider_id":       c.id,
			"native_session_id": tid,
			"transcript_id":     tid,
			"codex_thread_id":   tid,
			"live":              true,
			"state":             "running",
			"status":            "active",
			"updated_at":        at.UTC().Format(time.RFC3339Nano),
		})
	}
	for _, row := range c.desktopRuntimeSessions() {
		appendRow(row)
	}
	c.runtimeMu.Lock()
	for tid, at := range c.activeThreads {
		if codexActiveExpired(now, at) {
			delete(c.activeThreads, tid)
			continue
		}
		appendThread(tid, at)
	}
	c.runtimeMu.Unlock()
	c.clientMu.Lock()
	client := c.client
	c.clientMu.Unlock()
	c.sessMu.Lock()
	threads := map[string]string{}
	for sid, tid := range c.threads {
		threads[sid] = tid
	}
	c.sessMu.Unlock()
	if client != nil {
		for sid, tid := range threads {
			if tid == "" || !client.IsActive(tid) || seen[tid] {
				continue
			}
			appendRow(map[string]any{
				"session_id":        sid,
				"provider_id":       c.id,
				"native_session_id": tid,
				"transcript_id":     tid,
				"codex_thread_id":   tid,
				"live":              true,
				"state":             "running",
				"status":            "active",
				"updated_at":        now.UTC().Format(time.RFC3339Nano),
			})
		}
	}
	sortByUpdated(rows)
	return rows
}

func (c *Codex) PendingApprovalSessionIDs() []string {
	interrupting := c.interruptingThreadSet()
	c.approvalMu.Lock()
	ids := make([]string, 0, len(c.approvalsByThread))
	for threadID, pending := range c.approvalsByThread {
		if threadID == "" || interrupting[threadID] {
			continue
		}
		for _, req := range pending {
			if !req.Responding {
				ids = append(ids, threadID)
				break
			}
		}
	}
	c.approvalMu.Unlock()
	sort.Strings(ids)
	return ids
}

func (c *Codex) desktopRuntimeSessions() []map[string]any {
	if !c.desktopSyncEnabled {
		return nil
	}
	// The persistent bridge is the primary source of Desktop thread state;
	// the ephemeral snapshot listener remains as fallback (and for tests
	// injecting a fake desktop client).
	if b := c.desktopBridge(); b != nil {
		if rows := b.LiveThreadRows(); len(rows) > 0 {
			c.noteDesktopOwnerClients(rows)
			return rows
		}
	}
	snapshotter, ok := c.desktopClient().(interface {
		SnapshotLiveThreads(time.Duration) []map[string]any
	})
	if !ok {
		return nil
	}
	timeout := c.desktopIPCTimeout
	if timeout <= 0 || timeout > 1200*time.Millisecond {
		timeout = 1200 * time.Millisecond
	}
	rows := snapshotter.SnapshotLiveThreads(timeout)
	c.noteDesktopOwnerClients(rows)
	return rows
}

func (c *Codex) noteDesktopOwnerClients(rows []map[string]any) {
	if len(rows) == 0 {
		return
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	for _, row := range rows {
		tid := firstNonEmpty(stringAny(row["codex_thread_id"]), firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["native_session_id"])))
		owner := firstNonEmpty(stringAny(row["desktop_owner_client_id"]), stringAny(row["owner_client_id"]))
		if tid == "" || owner == "" {
			continue
		}
		c.desktopOwnerClients[tid] = owner
	}
}

func (c *Codex) desktopOwnerClient(threadID string) string {
	if threadID == "" {
		return ""
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	return c.desktopOwnerClients[threadID]
}

func (c *Codex) clearDesktopOwnerClient(threadID string) {
	if threadID == "" {
		return
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	delete(c.desktopOwnerClients, threadID)
}

func (c *Codex) isCurrentClientGeneration(generation uint64) bool {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	return generation != 0 && c.client != nil && c.clientGeneration == generation
}

func (c *Codex) currentClientRoute() (codexAppClient, uint64) {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	return c.client, c.clientGeneration
}

func (c *Codex) clientForGeneration(generation uint64) codexAppClient {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.client == nil || generation == 0 || c.clientGeneration != generation {
		return nil
	}
	return c.client
}

func (c *Codex) activateAppServerTurn(client codexAppClient, generation uint64, threadID string, turnID string) bool {
	if client == nil || generation == 0 || threadID == "" {
		return false
	}
	// Keep route activation atomic with respect to the exit callback. Once
	// clientMu is released, a later child exit will clear this exact active
	// route; an earlier exit makes the activation fail.
	c.clientMu.Lock()
	if c.client != client || c.clientGeneration != generation {
		c.clientMu.Unlock()
		return false
	}
	c.runtimeMu.Lock()
	c.appServerThreads[threadID] = true
	c.activeThreads[threadID] = time.Now()
	if turnID != "" {
		c.turnThreads[turnID] = threadID
	}
	c.runtimeMu.Unlock()
	c.sessMu.Lock()
	c.lastError = ""
	c.lastState = "running"
	c.lastChange = time.Now()
	c.sessMu.Unlock()
	c.clientMu.Unlock()
	return true
}

func (c *Codex) markAppServerThread(threadID string) {
	if threadID == "" {
		return
	}
	c.runtimeMu.Lock()
	c.appServerThreads[threadID] = true
	c.runtimeMu.Unlock()
}

func (c *Codex) markDesktopOwnedThread(threadID string) {
	if threadID == "" {
		return
	}
	c.runtimeMu.Lock()
	delete(c.appServerThreads, threadID)
	c.runtimeMu.Unlock()
}

func (c *Codex) markThreadInterrupting(threadID string, turnID string) {
	if threadID == "" {
		return
	}
	c.runtimeMu.Lock()
	c.interruptingThreads[threadID] = turnID
	c.runtimeMu.Unlock()
}

func (c *Codex) rollbackThreadInterrupting(threadID string) {
	if threadID == "" {
		return
	}
	c.runtimeMu.Lock()
	delete(c.interruptingThreads, threadID)
	c.runtimeMu.Unlock()
	c.publishApprovalChanged(threadID)
}

func (c *Codex) threadInterrupting(threadID string) bool {
	if threadID == "" {
		return false
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	_, ok := c.interruptingThreads[threadID]
	return ok
}

func (c *Codex) interruptingThreadSet() map[string]bool {
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	out := make(map[string]bool, len(c.interruptingThreads))
	for threadID := range c.interruptingThreads {
		out[threadID] = true
	}
	return out
}

func (c *Codex) finishThreadRoute(threadID string, turnID string) {
	if threadID == "" && turnID == "" {
		return
	}
	c.runtimeMu.Lock()
	if threadID == "" && turnID != "" {
		threadID = c.turnThreads[turnID]
	}
	if threadID != "" {
		delete(c.interruptingThreads, threadID)
	}
	if turnID != "" {
		delete(c.turnThreads, turnID)
	} else if threadID != "" {
		for candidate, owner := range c.turnThreads {
			if owner == threadID {
				delete(c.turnThreads, candidate)
			}
		}
	}
	c.runtimeMu.Unlock()
	if threadID != "" {
		c.sessMu.Lock()
		delete(c.pendingTools, threadID)
		c.sessMu.Unlock()
	}
}

func (c *Codex) clearAppServerRuntime() []string {
	c.runtimeMu.Lock()
	affected := []string{}
	owned := []string{}
	for threadID := range c.appServerThreads {
		owned = append(owned, threadID)
		if _, ok := c.activeThreads[threadID]; ok {
			affected = append(affected, threadID)
		}
		delete(c.activeThreads, threadID)
		delete(c.interruptingThreads, threadID)
	}
	for turnID, threadID := range c.turnThreads {
		if c.appServerThreads[threadID] {
			delete(c.turnThreads, turnID)
		}
	}
	c.runtimeMu.Unlock()
	c.sessMu.Lock()
	for _, threadID := range owned {
		delete(c.pendingTools, threadID)
	}
	c.sessMu.Unlock()
	sort.Strings(affected)
	return affected
}

func (c *Codex) setThreadActive(threadID string, active bool) {
	if threadID == "" {
		return
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	if active {
		c.activeThreads[threadID] = time.Now()
		return
	}
	delete(c.activeThreads, threadID)
}

func (c *Codex) threadActive(threadID string) bool {
	if threadID == "" {
		return false
	}
	now := time.Now()
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	at, ok := c.activeThreads[threadID]
	if !ok {
		return false
	}
	if codexActiveExpired(now, at) {
		delete(c.activeThreads, threadID)
		return false
	}
	return true
}

func (c *Codex) hasActiveThread() bool {
	now := time.Now()
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	for threadID, at := range c.activeThreads {
		if codexActiveExpired(now, at) {
			delete(c.activeThreads, threadID)
			continue
		}
		return true
	}
	return false
}

func (c *Codex) mergeRuntimeStatus(rows []map[string]any) {
	for _, row := range rows {
		tid := firstNonEmpty(stringAny(row["cli_session_id"]), stringAny(row["native_session_id"]))
		if tid == "" {
			continue
		}
		status := stringAny(row["status"])
		if status == "active" {
			c.setThreadActive(tid, true)
		}
		if c.threadActive(tid) {
			row["status"] = "active"
			row["state"] = "running"
			row["live"] = true
		} else if _, ok := row["live"]; !ok {
			row["live"] = false
		}
	}
}

func codexActiveExpired(now time.Time, at time.Time) bool {
	return !at.IsZero() && now.Sub(at) > 48*time.Hour
}

type codexAppClientLifecycle interface {
	Done() <-chan struct{}
	ExitError() error
}

type codexAppClientExitHandler interface {
	SetExitHandler(func(error))
}

func (c *Codex) Shutdown() {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	c.clientMu.Lock()
	client := c.client
	c.client = nil
	c.clientGeneration = 0
	c.clientMu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	// The app-server connection is explicitly gone: its pending approvals
	// can no longer be answered.
	c.clearAllAppServerApprovals()
	c.bridgeMu.Lock()
	bridge := c.bridge
	c.bridge = nil
	c.bridgeMu.Unlock()
	if bridge != nil {
		bridge.Stop()
	}
}

func (c *Codex) ensureClient() (codexAppClient, error) {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	for {
		c.clientMu.Lock()
		if c.client != nil {
			if lifecycle, ok := c.client.(codexAppClientLifecycle); ok {
				select {
				case <-lifecycle.Done():
					lost := c.client
					generation := c.clientGeneration
					err := lifecycle.ExitError()
					c.client = nil
					c.clientGeneration = 0
					c.clientMu.Unlock()
					// connectMu prevents a replacement generation from being
					// installed until all state owned by this generation is
					// cleared.
					c.handleAppServerLoss(generation, lost, err)
					continue
				default:
				}
			}
			client := c.client
			c.clientMu.Unlock()
			return client, nil
		}
		c.clientSequence++
		generation := c.clientSequence
		c.clientMu.Unlock()

		var client codexAppClient
		onNotification := func(method string, params map[string]any) {
			c.onNotificationFromClient(generation, method, params)
		}
		onServerRequest := func(requestID any, method string, params map[string]any) {
			c.onServerRequestFromClient(generation, requestID, method, params)
		}
		if c.clientFactory != nil {
			client = c.clientFactory(onNotification, onServerRequest)
		} else {
			if c.appServerTransport == "shared_daemon" {
				bin, err := c.resolveSharedDaemonCommand()
				if err != nil {
					return nil, err
				}
				status, queryErr := QueryCodexSharedDaemon(context.Background(), bin, codexSharedDaemonStatusTimeout)
				if queryErr != nil && c.sharedDaemonAutostart {
					if startErr := StartCodexSharedDaemon(context.Background(), bin, codexSharedDaemonMaxTimeout); startErr != nil {
						return nil, startErr
					}
					status, queryErr = QueryCodexSharedDaemon(context.Background(), bin, codexSharedDaemonStatusTimeout)
				}
				if queryErr != nil {
					return nil, queryErr
				}
				client = NewCodexSharedAppServerClient(
					status,
					c.cwd,
					onNotification,
					onServerRequest,
				)
			} else {
				bin := c.resolveCommand()
				if bin == "" {
					return nil, CodexAppServerError{"codex binary not found in ChatGPT.app, Codex.app, or PATH: " + c.command}
				}
				client = NewCodexAppServerClient(
					[]string{bin, "app-server"},
					c.cwd,
					onNotification,
					onServerRequest,
				)
			}
		}
		if lifecycle, ok := client.(codexAppClientExitHandler); ok {
			lifecycle.SetExitHandler(func(err error) {
				// Exit handlers may be invoked synchronously by a client while
				// the connection single-flight is still installing it.
				go c.onAppServerExit(generation, client, err)
			})
		}
		if err := client.Start(); err != nil {
			return nil, err
		}

		// Publish the generation before initialize so handshake notifications
		// can be handled. clientMu must not be held across Initialize: the read
		// loop invokes callbacks synchronously and would otherwise deadlock
		// before it could deliver the initialize response.
		c.clientMu.Lock()
		c.client = client
		c.clientGeneration = generation
		c.clientMu.Unlock()

		if err := client.Initialize("remote-agent"); err != nil {
			c.clientMu.Lock()
			detached := c.client == client && c.clientGeneration == generation
			if detached {
				c.client = nil
				c.clientGeneration = 0
			}
			c.clientMu.Unlock()
			_ = client.Close()
			if detached {
				c.handleAppServerLoss(generation, client, err)
			}
			return nil, err
		}

		c.setLastError("")
		if c.getLastState() == "error" {
			if c.hasActiveThread() {
				c.setLastState("running")
			} else {
				c.setLastState("idle")
			}
		}
		go c.seedAccount(client)
		return client, nil
	}
}

func (c *Codex) onAppServerExit(generation uint64, client codexAppClient, err error) {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	c.clientMu.Lock()
	if c.client != client || c.clientGeneration != generation {
		c.clientMu.Unlock()
		return
	}
	c.client = nil
	c.clientGeneration = 0
	c.clientMu.Unlock()
	c.handleAppServerLoss(generation, client, err)
}

func (c *Codex) handleAppServerLoss(generation uint64, client codexAppClient, err error) {
	affected := c.clearAppServerRuntime()
	approvalThreads := c.appServerApprovalThreadIDs()
	c.clearAllAppServerApprovals()
	for _, threadID := range approvalThreads {
		c.publishApprovalChanged(threadID)
	}
	if err == nil {
		err = CodexAppServerError{"codex app-server exited"}
	}
	c.setLastError("codex app-server connection lost: " + err.Error())
	c.setLastState("error")
	for _, threadID := range affected {
		if !stringIn(approvalThreads, threadID) {
			c.publishApprovalChanged(threadID)
		}
	}
}

func (c *Codex) seedAccount(client codexAppClient) {
	if res, err := client.AccountRateLimits(10 * time.Second); err == nil {
		if m := mapAny(res); len(m) > 0 {
			c.sessMu.Lock()
			c.rateLimits = mapAny(firstValue(m["rateLimits"], res))
			c.rateLimitsByID = mapAny(m["rateLimitsByLimitId"])
			c.sessMu.Unlock()
		}
	}
	if res, err := client.AccountRead(10 * time.Second); err == nil {
		acc := mapAny(mapAny(res)["account"])
		c.sessMu.Lock()
		c.planType = stringAny(acc["planType"])
		c.sessMu.Unlock()
	}
}

func (c *Codex) threadFor(sessionID string) string {
	if tid := c.boundThread(sessionID); tid != "" {
		return tid
	}
	tid, err := c.OpenOrCreateSession(sessionID, StartOptions{})
	if err != nil {
		return ""
	}
	return tid
}

func (c *Codex) resumeThread(client codexAppClient, sessionID string, threadID string, cwd string) (string, error) {
	targetCwd := firstNonEmpty(cwd, c.cwd)
	res, err := client.ThreadResume(threadID, map[string]any{"cwd": targetCwd}, c.appServerResumeTimeout())
	if err != nil {
		return "", err
	}
	thread := mapAny(mapAny(res)["thread"])
	tid := stringAny(thread["id"])
	if tid == "" {
		return "", errors.New("thread/resume returned no thread id")
	}
	if tid != threadID {
		return "", fmt.Errorf("thread/resume route mismatch: requested %s, received %s", threadID, tid)
	}
	returnedCwd := firstNonEmpty(stringAny(thread["cwd"]), stringAny(mapAny(res)["cwd"]))
	if returnedCwd != "" && !sameCodexCwd(returnedCwd, targetCwd) {
		return "", fmt.Errorf(
			"thread/resume cwd mismatch for %s: requested %q, received %q",
			threadID,
			targetCwd,
			returnedCwd,
		)
	}
	c.bindThread(sessionID, tid)
	opts := c.startOptionsFor(sessionID)
	if opts == nil {
		opts = map[string]any{}
	}
	opts["cwd"] = targetCwd
	c.setStartOptions(sessionID, opts)
	status := firstNonEmpty(
		stringAny(mapAny(thread["status"])["type"]),
		stringAny(thread["status"]),
	)
	if status == "" {
		status = "idle"
	}
	activeTurnID, activeTurns := codexResumedActiveTurn(thread)
	if activeTurns > 1 || (status == "active" && activeTurns != 1) ||
		(status != "active" && activeTurns != 0) {
		return "", fmt.Errorf("thread/resume returned inconsistent active turn state for %s", threadID)
	}
	client.SetThreadStatus(tid, status)
	c.setThreadActive(tid, status == "active")
	if activeTurnID != "" {
		client.SetThreadTurn(tid, activeTurnID)
		c.bindTurnThread(tid, activeTurnID)
	}
	return tid, nil
}

func sameCodexCwd(left string, right string) bool {
	return comparableCodexCwd(left) == comparableCodexCwd(right)
}

func (c *Codex) appServerResumeTimeout() time.Duration {
	if c.appServerTransport == "stdio" {
		return codexAppServerStdioResumeTimeout
	}
	return codexAppServerMutationTimeout
}

func comparableCodexCwd(path string) string {
	clean := filepath.Clean(expandUser(path))
	probe := clean
	suffix := []string{}
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return clean
		}
		suffix = append(suffix, filepath.Base(probe))
		probe = parent
	}
}

func codexResumedActiveTurn(thread map[string]any) (string, int) {
	activeID := ""
	count := 0
	for _, turn := range codexThreadTurns(thread) {
		status := strings.ToLower(firstNonEmpty(
			stringAny(mapAny(turn["status"])["type"]),
			stringAny(turn["status"]),
		))
		switch status {
		case "active", "running", "inprogress", "in_progress":
			count++
			activeID = stringAny(turn["id"])
		}
	}
	return activeID, count
}

func (c *Codex) threadStartOptions(cwd string, model string, effort string, mode string) map[string]any {
	approvalPolicy := c.approvalPolicy
	sandbox := c.sandbox
	switch mode {
	case "auto":
		approvalPolicy = "never"
	case "edit":
		approvalPolicy = "on-request"
	case "plan":
		approvalPolicy = "on-request"
		sandbox = "read-only"
	case "ask", "default":
		approvalPolicy = "on-request"
	case "never", "on-request":
		approvalPolicy = mode
	}
	c.sessMu.Lock()
	nextModel, nextEffort := c.nextModel, c.nextEffort
	c.sessMu.Unlock()
	return compactMap(map[string]any{
		"cwd":             firstNonEmpty(cwd, c.cwd),
		"approvalPolicy":  approvalPolicy,
		"sandbox":         sandbox,
		"model":           firstNonEmpty(model, nextModel),
		"reasoningEffort": firstNonEmpty(effort, nextEffort),
	})
}

func (c *Codex) modeName() string {
	return c.modeNameFor(c.approvalPolicy)
}

func (c *Codex) modeNameFor(policy string) string {
	if policy == "never" {
		return "auto"
	}
	return policy
}

func (c *Codex) resolveCommand() string {
	if c.preferDesktopCodex {
		if p := resolveDesktopCodex(nil); p != "" {
			return p
		}
	}
	if filepath.IsAbs(c.command) {
		if st, err := os.Stat(c.command); err == nil && !st.IsDir() {
			return c.command
		}
		return ""
	}
	if p, err := exec.LookPath(strings.Fields(c.command)[0]); err == nil {
		return p
	}
	return ""
}

func (c *Codex) resolveSharedDaemonCommand() (string, error) {
	candidates := []string{}
	if configured := strings.TrimSpace(c.sharedDaemonCommand); configured != "" {
		candidates = append(candidates, expandUser(configured))
	} else {
		if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
			candidates = append(candidates, filepath.Join(expandUser(codexHome), "packages", "standalone", "current", "codex"))
		}
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			candidates = append(candidates, filepath.Join(home, ".codex", "packages", "standalone", "current", "codex"))
		}
	}
	var lastErr error
	for _, candidate := range dedupeStrings(candidates) {
		resolved, err := validateCodexSharedDaemonExecutable(candidate)
		if err == nil {
			return resolved, nil
		}
		lastErr = err
	}
	if strings.TrimSpace(c.sharedDaemonCommand) != "" && lastErr != nil {
		return "", fmt.Errorf("configured Codex shared daemon command is unsafe: %w", lastErr)
	}
	return "", errors.New("Codex shared app-server requires the managed standalone install at ~/.codex/packages/standalone/current/codex")
}

func resolveDesktopCodex(candidates []string) string {
	if len(candidates) == 0 {
		candidates = []string{
			"/Applications/ChatGPT.app", "~/Applications/ChatGPT.app",
			"/Applications/Codex.app", "~/Applications/Codex.app",
		}
	}
	for _, app := range candidates {
		bin := filepath.Join(expandUser(app), "Contents", "Resources", "codex")
		if st, err := os.Stat(bin); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return bin
		}
	}
	return ""
}

func (c *Codex) tryDesktopStartTurn(sessionID string, threadID string, prompt string) (string, error) {
	return c.tryDesktopStartTurnWithAttachments(sessionID, threadID, prompt, nil)
}

func (c *Codex) tryDesktopStartTurnWithAttachments(sessionID string, threadID string, prompt string, attachments []Attachment) (string, error) {
	client := c.desktopClient()
	var res any
	startOpts := c.startOptionsFor(sessionID)
	var err error
	if len(attachments) > 0 {
		targeted, ok := client.(interface {
			StartTurnOnClientWithAttachments(string, string, []Attachment, map[string]any, string, time.Duration) (any, error)
		})
		if !ok {
			return "", errors.New("Codex Desktop IPC does not support attachments")
		}
		owner, ownerErr := c.requireDesktopOwnerClient(threadID)
		if ownerErr != nil {
			return "", ownerErr
		}
		res, err = targeted.StartTurnOnClientWithAttachments(threadID, prompt, attachments, startOpts, owner, c.desktopIPCTimeout)
		if err != nil && isDesktopNoClientFound(err) {
			refreshed := c.refreshDesktopOwnerClient(threadID)
			if refreshed != "" && refreshed != owner {
				res, err = targeted.StartTurnOnClientWithAttachments(threadID, prompt, attachments, startOpts, refreshed, c.desktopIPCTimeout)
			}
			if err != nil && isDesktopNoClientFound(err) {
				c.clearDesktopOwnerClient(threadID)
				err = errNoDesktopOwnerClient()
			}
		}
	} else if targeted, ok := client.(interface {
		StartTurnOnClient(string, string, map[string]any, string, time.Duration) (any, error)
	}); ok {
		owner, ownerErr := c.requireDesktopOwnerClient(threadID)
		if ownerErr != nil {
			return "", ownerErr
		}
		res, err = targeted.StartTurnOnClient(threadID, prompt, startOpts, owner, c.desktopIPCTimeout)
		if err != nil && isDesktopNoClientFound(err) {
			refreshed := c.refreshDesktopOwnerClient(threadID)
			if refreshed != "" && refreshed != owner {
				res, err = targeted.StartTurnOnClient(threadID, prompt, startOpts, refreshed, c.desktopIPCTimeout)
			}
			if err != nil && isDesktopNoClientFound(err) {
				c.clearDesktopOwnerClient(threadID)
				err = errNoDesktopOwnerClient()
			}
		}
	} else {
		err = errors.New("Codex Desktop IPC does not support owner-targeted start")
	}
	if err != nil {
		return "", err
	}
	return responseTurnID(res), nil
}

func (c *Codex) tryDesktopSteerTurn(sessionID string, threadID string, prompt string) error {
	client := c.desktopClient()
	var err error
	if targeted, ok := client.(interface {
		SteerTurnOnClient(string, string, string, time.Duration) (any, error)
	}); ok {
		owner, ownerErr := c.requireDesktopOwnerClient(threadID)
		if ownerErr != nil {
			return ownerErr
		}
		_, err = targeted.SteerTurnOnClient(threadID, prompt, owner, c.desktopIPCTimeout)
		if err != nil && isDesktopNoClientFound(err) {
			refreshed := c.refreshDesktopOwnerClient(threadID)
			if refreshed != "" && refreshed != owner {
				_, err = targeted.SteerTurnOnClient(threadID, prompt, refreshed, c.desktopIPCTimeout)
			}
			if err != nil && isDesktopNoClientFound(err) {
				c.clearDesktopOwnerClient(threadID)
				err = errNoDesktopOwnerClient()
			}
		}
	} else {
		err = errors.New("Codex Desktop IPC does not support owner-targeted steer")
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *Codex) tryDesktopInterruptTurn(sessionID string, threadID string) error {
	client := c.desktopClient()
	var err error
	if targeted, ok := client.(interface {
		InterruptTurnOnClient(string, string, time.Duration) (any, error)
	}); ok {
		owner, ownerErr := c.requireDesktopOwnerClient(threadID)
		if ownerErr != nil {
			return ownerErr
		}
		_, err = targeted.InterruptTurnOnClient(threadID, owner, c.desktopIPCTimeout)
		if err != nil && isDesktopNoClientFound(err) {
			refreshed := c.refreshDesktopOwnerClient(threadID)
			if refreshed != "" && refreshed != owner {
				_, err = targeted.InterruptTurnOnClient(threadID, refreshed, c.desktopIPCTimeout)
			}
			if err != nil && isDesktopNoClientFound(err) {
				c.clearDesktopOwnerClient(threadID)
				err = errNoDesktopOwnerClient()
			}
		}
	} else {
		err = errors.New("Codex Desktop IPC does not support owner-targeted interrupt")
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *Codex) refreshDesktopOwnerClient(threadID string) string {
	c.clearDesktopOwnerClient(threadID)
	_ = c.desktopRuntimeSessions()
	return c.desktopOwnerClient(threadID)
}

func (c *Codex) ensureDesktopOwnerClient(threadID string) string {
	if owner := c.desktopOwnerClient(threadID); owner != "" {
		return owner
	}
	return c.refreshDesktopOwnerClient(threadID)
}

func (c *Codex) attachDesktopOwner(threadID string) error {
	if !c.desktopSyncEnabled || !codexThreadIDRE.MatchString(threadID) {
		return nil
	}
	if owner := c.desktopOwnerClient(threadID); owner != "" {
		return nil
	}
	if err := c.openDesktopThread(threadID); err != nil {
		return fmt.Errorf("open Codex Desktop thread: %w", err)
	}
	timeout := c.desktopAttachTimeout
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if owner := c.ensureDesktopOwnerClient(threadID); owner != "" {
			return nil
		}
		if time.Now().After(deadline) {
			if c.desktopFactory == nil {
				conn, socketErr := dialCodexDesktopSocket(c.desktopIPCSocket, 500*time.Millisecond)
				if conn != nil {
					_ = conn.Close()
				}
				if socketErr != nil {
					return fmt.Errorf("codex Desktop IPC unavailable: %w", socketErr)
				}
			}
			return errNoDesktopOwnerClient()
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (c *Codex) openDesktopThread(threadID string) error {
	if c.desktopOpener != nil {
		return c.desktopOpener(threadID)
	}
	return exec.Command("open", "codex://threads/"+url.PathEscape(threadID)).Run()
}

func (c *Codex) requireDesktopOwnerClient(threadID string) (string, error) {
	owner := c.ensureDesktopOwnerClient(threadID)
	if owner == "" {
		return "", errNoDesktopOwnerClient()
	}
	return owner, nil
}

func errNoDesktopOwnerClient() error {
	return errors.New("codex thread has no active Desktop IPC owner; reattach the thread in Codex Desktop")
}

func isNoDesktopOwnerClient(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no active Desktop IPC owner")
}

func isDesktopNoClientFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no-client-found")
}

func (c *Codex) desktopClient() codexDesktopClient {
	if c.desktopFactory != nil {
		return c.desktopFactory()
	}
	return NewCodexDesktopIPCClient(c.desktopIPCSocket, c.desktopIPCTimeout, c.desktopIPCHostID)
}

func (c *Codex) shouldTryDesktopSync(sessionID string, threadID string) bool {
	return c.desktopSyncEnabled && c.desktopSyncBound(sessionID) && codexThreadIDRE.MatchString(threadID)
}

func (c *Codex) threadForDelivery(sessionID string) (string, error) {
	c.sessMu.Lock()
	bound := c.threads[sessionID]
	route := c.sessionRoutes[sessionID]
	c.sessMu.Unlock()
	if route == "unavailable" {
		return "", errors.New("native Codex delivery requires shared daemon or explicit Desktop IPC")
	}
	tid := firstNonEmpty(bound, sessionID)
	if tid == "" {
		return "", errors.New("no codex thread")
	}
	if bound == "" {
		c.bindThread(sessionID, tid)
	}
	if bound == "" && sessionID == tid && codexThreadIDRE.MatchString(tid) {
		c.markDesktopSyncCandidate(sessionID, tid)
	}
	return tid, nil
}

// startTurn honors the owner route bound before delivery. Shared-daemon
// sessions resume and write on that connection; the explicit Desktop
// compatibility route lazily opens the renderer and targets its published IPC
// owner. A request is never retried across those routes after delivery may
// have occurred.
func (c *Codex) startTurn(sessionID string, threadID string, prompt string) (string, string, error) {
	return c.startTurnWithAttachments(sessionID, threadID, prompt, nil)
}

func (c *Codex) startTurnWithAttachments(sessionID string, threadID string, prompt string, attachments []Attachment) (string, string, error) {
	return c.startTurnWithBudget(sessionID, threadID, prompt, attachments, 0, true)
}

func (c *Codex) startTurnWithBudget(
	sessionID string,
	threadID string,
	prompt string,
	attachments []Attachment,
	timeout time.Duration,
	retryThreadNotFound bool,
) (string, string, error) {
	if c.shouldTryDesktopSync(sessionID, threadID) {
		if c.ensureDesktopOwnerClient(threadID) == "" {
			if err := c.attachDesktopOwner(threadID); err != nil {
				return "", "", err
			}
		}
		turnID, err := c.tryDesktopStartTurnWithAttachments(sessionID, threadID, prompt, attachments)
		if err == nil {
			c.markDesktopOwnedThread(threadID)
			c.bindTurnThread(threadID, turnID)
			return turnID, "Codex Desktop", nil
		}
		// The cached owner can disappear between discovery and the targeted
		// request. no-client-found proves that attempt was not delivered, so it
		// is safe to reopen the thread and retry once on its new owner.
		if isNoDesktopOwnerClient(err) {
			if attachErr := c.attachDesktopOwner(threadID); attachErr != nil {
				return "", "", attachErr
			}
			turnID, err = c.tryDesktopStartTurnWithAttachments(sessionID, threadID, prompt, attachments)
			if err == nil {
				c.markDesktopOwnedThread(threadID)
				c.bindTurnThread(threadID, turnID)
				return turnID, "Codex Desktop", nil
			}
		}
		return "", "", err
	}
	client, err := c.ensureClient()
	if err != nil {
		return "", "", err
	}
	_, generation := c.currentClientRoute()
	if c.appServerResumeBound(sessionID) {
		resumeCwd := stringAny(c.startOptionsFor(sessionID)["cwd"])
		if _, err := c.resumeThread(client, sessionID, threadID, resumeCwd); err != nil {
			return "", "", err
		}
		if client.IsActive(threadID) {
			return "", "", errors.New("a turn is already in progress for this codex thread")
		}
	}
	c.markAppServerThread(threadID)
	start := func() (any, error) {
		if timeout > 0 {
			if timed, ok := client.(interface {
				TurnStartWithAttachmentsTimeout(string, string, []Attachment, map[string]any, time.Duration) (any, error)
			}); ok {
				return timed.TurnStartWithAttachmentsTimeout(
					threadID,
					prompt,
					attachments,
					c.startOptionsFor(sessionID),
					timeout,
				)
			}
		}
		if len(attachments) == 0 {
			return client.TurnStart(threadID, prompt, c.startOptionsFor(sessionID))
		}
		withAttachments, ok := client.(interface {
			TurnStartWithAttachments(string, string, []Attachment, map[string]any) (any, error)
		})
		if !ok {
			return nil, errors.New("Codex app-server does not support attachments")
		}
		return withAttachments.TurnStartWithAttachments(threadID, prompt, attachments, c.startOptionsFor(sessionID))
	}
	res, err := start()
	if err != nil && retryThreadNotFound && isThreadNotFound(err) {
		if _, resumeErr := c.resumeThread(client, sessionID, threadID, ""); resumeErr != nil {
			return "", "", resumeErr
		}
		res, err = start()
	}
	if err != nil {
		if codexTurnStartOutcomeUnknown(err) {
			c.markAppServerSessionNeedsResume(sessionID)
			return "", "", fmt.Errorf(
				"%w; turn/start delivery outcome is unknown and the next send will reconcile this thread first",
				err,
			)
		}
		return "", "", err
	}
	turnID := responseTurnID(res)
	if turnID != "" {
		client.SetThreadTurn(threadID, turnID)
	}
	if !c.activateAppServerTurn(client, generation, threadID, turnID) {
		c.markAppServerSessionNeedsResume(sessionID)
		return "", "", errors.New("Codex app-server exited after turn/start acknowledgement; terminal state is unknown")
	}
	return turnID, "Codex app-server", nil
}

func codexTurnStartOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout waiting for turn/start") ||
		strings.Contains(message, "write to codex app-server failed") ||
		strings.Contains(message, `"code":-32099`)
}

func (c *Codex) markDesktopSyncCandidate(sessionID string, threadID string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	if c.desktopSyncEnabled && codexThreadIDRE.MatchString(threadID) {
		c.desktopSyncSessions[sessionID] = true
		delete(c.appServerResumeSessions, sessionID)
		c.sessionRoutes[sessionID] = "desktop_ipc"
	} else {
		delete(c.desktopSyncSessions, sessionID)
	}
}

func (c *Codex) onNotificationFromClient(generation uint64, method string, params map[string]any) {
	if !c.isCurrentClientGeneration(generation) {
		return
	}
	c.onNotification(method, params)
}

func (c *Codex) onNotification(method string, params map[string]any) {
	threadID := c.threadIDForNotification(params)
	c.trackThreadRuntime(method, params, threadID)
	c.publishStreamNotification(method, params, threadID)
	switch method {
	case "thread/status/changed":
		status := stringAny(mapAny(params["status"])["type"])
		if status == "active" {
			c.setLastState("running")
		} else if codexThreadStatusTerminal(status) {
			c.setLastState("idle")
			// Scope cleanup to the thread that went idle; approvals pending
			// on other threads must survive.
			c.clearAppServerApprovalsForThread(firstNonEmpty(stringAny(params["threadId"]), threadID))
		}
	case "serverRequest/resolved":
		// The server resolved one of its own requests (answered elsewhere,
		// auto-reviewed, or cancelled): drop exactly that approval.
		resolvedThread := firstNonEmpty(stringAny(params["threadId"]), threadID)
		if c.removeAppServerApproval(resolvedThread, typedCodexRequestKey(params["requestId"])) != nil {
			c.publishApprovalChanged(resolvedThread)
		}
	case "turn/completed":
		status := codexTurnCompletionStatus(params)
		if status == "failed" {
			c.setLastState("error")
			if message := codexTurnCompletionError(params); message != "" {
				c.setLastError(message)
			}
		} else {
			c.setLastError("")
			c.setLastState("idle")
		}
	case "account/rateLimits/updated":
		if rl := mapAny(params["rateLimits"]); len(rl) > 0 {
			c.sessMu.Lock()
			c.rateLimits = rl
			c.sessMu.Unlock()
		}
	case "account/updated":
		if pt := stringAny(params["planType"]); pt != "" {
			c.sessMu.Lock()
			c.planType = pt
			c.sessMu.Unlock()
		}
	}
}

func (c *Codex) trackThreadRuntime(method string, params map[string]any, threadID string) {
	switch method {
	case "thread/status/changed":
		status := stringAny(mapAny(params["status"])["type"])
		if status == "active" {
			c.setThreadActive(threadID, true)
		} else if codexThreadStatusTerminal(status) {
			c.setThreadActive(threadID, false)
			c.finishThreadRoute(threadID, "")
		}
	case "turn/completed":
		c.setThreadActive(threadID, false)
		turnID := firstNonEmpty(stringAny(params["turnId"]), stringAny(mapAny(params["turn"])["id"]))
		c.finishThreadRoute(threadID, turnID)
		// A finished turn cannot have live approval callbacks.
		c.clearAppServerApprovalsForThread(firstNonEmpty(stringAny(params["threadId"]), threadID))
	}
}

func codexThreadStatusTerminal(status string) bool {
	switch strings.ToLower(status) {
	case "idle", "completed", "failed", "canceled", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}

func codexTurnCompletionStatus(params map[string]any) string {
	turn := mapAny(params["turn"])
	return strings.ToLower(firstNonEmpty(stringAny(turn["status"]), stringAny(params["status"])))
}

func codexTurnCompletionError(params map[string]any) string {
	turn := mapAny(params["turn"])
	errPayload := firstValue(turn["error"], params["error"])
	if message := stringAny(mapAny(errPayload)["message"]); message != "" {
		return message
	}
	return stringAny(errPayload)
}

func (c *Codex) publishStreamNotification(method string, params map[string]any, threadID string) {
	if c.streamPublisher == nil {
		return
	}
	frames := c.framesForNotification(method, params, threadID)
	if len(frames) == 0 {
		return
	}
	targets := []string{}
	if sid := c.sessionForThread(threadID); sid != "" {
		targets = append(targets, sid)
	}
	if threadID != "" && !stringIn(targets, threadID) {
		targets = append(targets, threadID)
	}
	for _, target := range targets {
		for _, frame := range frames {
			c.streamPublisher(target, frame)
		}
	}
}

func (c *Codex) threadIDForNotification(params map[string]any) string {
	item := mapAny(params["item"])
	turnID := firstNonEmpty(firstNonEmpty(stringAny(params["turnId"]), stringAny(item["turnId"])), stringAny(mapAny(params["turn"])["id"]))
	threadID := firstNonEmpty(
		firstNonEmpty(stringAny(params["threadId"]), stringAny(params["thread_id"])),
		firstNonEmpty(stringAny(item["threadId"]), stringAny(item["thread_id"])),
	)
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	if threadID == "" && turnID != "" {
		threadID = c.turnThreads[turnID]
	}
	if threadID != "" && turnID != "" {
		c.turnThreads[turnID] = threadID
	}
	return threadID
}

func (c *Codex) bindTurnThread(threadID string, turnID string) {
	if threadID == "" || turnID == "" {
		return
	}
	c.runtimeMu.Lock()
	c.turnThreads[turnID] = threadID
	c.runtimeMu.Unlock()
}

func (c *Codex) framesForNotification(method string, params map[string]any, threadID string) []map[string]any {
	switch method {
	case "item/agentMessage/delta":
		text := firstNonEmpty(stringAny(params["delta"]), stringAny(params["text"]))
		if text == "" {
			return nil
		}
		return []map[string]any{{"type": "delta", "turn_id": nullableString(params["turnId"]), "text": text}}
	case "item/agentReasoning/delta":
		text := firstNonEmpty(stringAny(params["delta"]), stringAny(params["text"]))
		if text == "" {
			return nil
		}
		return []map[string]any{{"type": "item", "item": map[string]any{
			"role": "assistant", "kind": "thinking", "text": text,
			"thread_id": threadID, "turn_id": nullableString(params["turnId"]), "item_id": nullableString(params["itemId"]),
		}}}
	case "thread/status/changed":
		status := stringAny(mapAny(params["status"])["type"])
		turnID := firstNonEmpty(stringAny(params["turnId"]), stringAny(mapAny(params["turn"])["id"]))
		if status == "active" {
			return []map[string]any{{"type": "turn", "status": "started", "turn_id": nullableNonEmpty(turnID)}}
		}
		if status == "idle" || status == "completed" {
			return []map[string]any{{"type": "turn", "status": "completed", "turn_id": nullableNonEmpty(turnID)}}
		}
	case "turn/completed":
		return []map[string]any{{"type": "turn", "status": "completed", "turn_id": nullableString(params["turnId"])}}
	default:
		if strings.HasPrefix(method, "item/") {
			if item := mapAny(params["item"]); len(item) > 0 {
				return c.framesForItem(item, threadID, stringAny(params["turnId"]))
			}
		}
	}
	return nil
}

func (c *Codex) framesForItem(item map[string]any, threadID string, turnID string) []map[string]any {
	if threadID == "" {
		return nil
	}
	c.sessMu.Lock()
	if c.pendingTools[threadID] == nil {
		c.pendingTools[threadID] = map[string]map[string]any{}
	}
	pending := c.pendingTools[threadID]
	cid := codexCallID(item)
	target := pending[cid]
	messages := codexItemToMessages(item, "", pending, "", threadID, firstNonEmpty(turnID, stringAny(item["turnId"])))
	c.sessMu.Unlock()
	if target != nil {
		return []map[string]any{{"type": "item_update", "item": target}}
	}
	out := []map[string]any{}
	for _, msg := range messages {
		out = append(out, map[string]any{"type": "item", "item": msg})
	}
	return out
}

func (c *Codex) onServerRequestFromClient(generation uint64, requestID any, method string, params map[string]any) {
	if !c.isCurrentClientGeneration(generation) {
		return
	}
	c.handleServerRequest(generation, requestID, method, params)
}

func (c *Codex) onServerRequest(requestID any, method string, params map[string]any) {
	_, generation := c.currentClientRoute()
	c.handleServerRequest(generation, requestID, method, params)
}

func (c *Codex) handleServerRequest(generation uint64, requestID any, method string, params map[string]any) {
	if method == "item/tool/call" {
		_ = c.answerDynamicTool(generation, requestID, params)
		return
	}
	// Only genuine human requests become approvals. Machine requests (token
	// refresh, attestation, clock reads, ...) get an immediate JSON-RPC
	// "method not found" so the server neither blocks nor shows up as a
	// phantom approval.
	if !codexHumanRequestMethod(method) {
		client := c.clientForGeneration(generation)
		if client != nil {
			_ = client.RespondError(requestID, -32601, "method not supported by remote-agent: "+method)
		}
		return
	}
	// Some app-server protocol revisions omit threadId from the human request
	// and only include turnId. Recover the authoritative thread mapping before
	// indexing the approval, otherwise it lands under the empty-session bucket
	// and never appears in the PWA for the owning thread.
	if codexRequestThreadID(params) == "" {
		if threadID := c.threadIDForNotification(params); threadID != "" {
			copyParams := make(map[string]any, len(params)+1)
			for k, v := range params {
				copyParams[k] = v
			}
			copyParams["threadId"] = threadID
			params = copyParams
		}
	}
	if threadID := codexRequestThreadID(params); c.threadInterrupting(threadID) {
		if client := c.clientForGeneration(generation); client != nil {
			_ = client.RespondError(requestID, -32800, "turn cancellation already requested")
		}
		return
	}
	threadID := c.addAppServerApproval(generation, requestID, method, params)
	c.setLastState("waiting_approval")
	c.publishApprovalChanged(threadID)
}

func (c *Codex) answerDynamicTool(generation uint64, requestID any, params map[string]any) error {
	client := c.clientForGeneration(generation)
	if client == nil {
		return errors.New("codex app-server request belongs to a stale connection")
	}
	ns := stringAny(params["namespace"])
	tool := stringAny(params["tool"])
	if ns == "codex_app" && tool == "load_workspace_dependencies" {
		return client.Respond(requestID, map[string]any{
			"success":      true,
			"contentItems": []map[string]any{{"type": "inputText", "text": workspaceDependenciesText()}},
		})
	}
	label := tool
	if ns != "" {
		label = ns + "." + tool
	}
	return client.Respond(requestID, map[string]any{
		"success":      false,
		"contentItems": []map[string]any{{"type": "inputText", "text": "Unsupported dynamic tool in remote-agent: " + label}},
	})
}

func (c *Codex) sessionForThread(threadID string) string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	logical := []string{}
	for sid, tid := range c.threads {
		if tid == threadID {
			if sid != threadID {
				logical = append(logical, sid)
			}
		}
	}
	if len(logical) > 0 {
		sort.Strings(logical)
		return logical[0]
	}
	if c.threads[threadID] == threadID {
		return threadID
	}
	return ""
}

// --- locked session/state accessors ------------------------------------------

func (c *Codex) threadForSession(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return firstNonEmpty(c.threads[sessionID], sessionID)
}

func (c *Codex) boundThread(sessionID string) string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.threads[sessionID]
}

func (c *Codex) bindThread(sessionID string, threadID string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.threads[sessionID] = threadID
}

func (c *Codex) startOptionsFor(sessionID string) map[string]any {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sessionStartOptions[sessionID]
}

func (c *Codex) setStartOptions(sessionID string, opts map[string]any) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.sessionStartOptions[sessionID] = opts
}

func (c *Codex) desktopSyncBound(sessionID string) bool {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.desktopSyncSessions[sessionID]
}

func (c *Codex) sessionRoute(sessionID string) string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sessionRoutes[sessionID]
}

func (c *Codex) appServerResumeBound(sessionID string) bool {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.appServerResumeSessions[sessionID]
}

func (c *Codex) markAppServerSessionReady(sessionID string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	delete(c.appServerResumeSessions, sessionID)
}

func (c *Codex) markAppServerSessionNeedsResume(sessionID string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.appServerResumeSessions[sessionID] = true
}

func (c *Codex) setLastState(state string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.lastState = state
	c.lastChange = time.Now()
}

func (c *Codex) getLastState() string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.lastState
}

func (c *Codex) setLastError(msg string) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.lastError = msg
}

func (c *Codex) getLastError() string {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.lastError
}

// --- Desktop IPC bridge wiring -----------------------------------------------

// desktopBridge lazily starts the persistent Desktop IPC follower bridge in
// real deployments. Tests inject a pre-populated bridge (or none) directly.
func (c *Codex) desktopBridge() *codexDesktopBridge {
	if !c.desktopSyncEnabled {
		return nil
	}
	c.bridgeMu.Lock()
	defer c.bridgeMu.Unlock()
	if c.bridge != nil {
		return c.bridge
	}
	if c.desktopFactory != nil {
		// Test/fake desktop client: no real socket to bridge.
		return nil
	}
	c.bridge = newCodexDesktopBridge(c.desktopIPCSocket, c.desktopIPCHostID)
	c.bridge.onHumanRequestsChanged = c.publishApprovalChanged
	c.bridge.Start()
	return c.bridge
}

func (c *Codex) scheduleDesktopPendingRefresh(threadID string) {
	if !c.desktopSyncEnabled || !codexThreadIDRE.MatchString(threadID) {
		return
	}
	b := c.desktopBridge()
	if b == nil || b.HasThread(threadID) {
		return
	}
	c.desktopRefreshMu.Lock()
	if last := c.desktopRefreshAt[threadID]; !last.IsZero() && time.Since(last) < 10*time.Second {
		c.desktopRefreshMu.Unlock()
		return
	}
	c.desktopRefreshAt[threadID] = time.Now()
	c.desktopRefreshMu.Unlock()
	go func() {
		if c.ensureDesktopOwnerClient(threadID) != "" {
			c.refreshDesktopPendingState(threadID)
		}
	}()
}

func (c *Codex) publishApprovalChanged(threadID string) {
	if c.streamPublisher == nil || threadID == "" {
		return
	}
	frame := map[string]any{"type": "approval_changed"}
	targets := []string{threadID}
	if sid := c.sessionForThread(threadID); sid != "" && sid != threadID {
		targets = append(targets, sid)
	}
	for _, target := range targets {
		c.streamPublisher(target, frame)
	}
}

func (c *Codex) refreshDesktopPendingState(threadID string) bool {
	b := c.desktopBridge()
	if b == nil || threadID == "" {
		return false
	}
	owner := c.desktopOwnerClient(threadID)
	if owner == "" {
		owner = b.OwnerClient(threadID)
	}
	return b.RefreshThread(threadID, owner, 1500*time.Millisecond)
}

// SessionSettings reports the session's real effective settings as owned by
// Codex Desktop (approval policy, reviewer, sandbox, model, effort) so
// /status reflects the rollout truth instead of provider defaults.
func (c *Codex) SessionSettings(sessionID string) map[string]any {
	if !c.desktopSyncBound(sessionID) {
		return nil
	}
	tid := c.threadForSession(sessionID)
	if tid == "" || !codexThreadIDRE.MatchString(tid) {
		return nil
	}
	b := c.desktopBridge()
	if b == nil {
		return nil
	}
	st := b.ThreadSettings(tid)
	if st == nil {
		return nil
	}
	if policy := stringAny(st["approval_policy"]); policy != "" {
		st["mode"] = c.modeNameFor(policy)
	}
	return st
}

// codexDesktopApprovalResponder is the Desktop IPC surface used to answer
// approvals owned by a Desktop client.
type codexDesktopApprovalResponder interface {
	CommandApprovalDecision(conversationID string, requestID any, decision any, targetClientID string, timeout time.Duration) (any, error)
	FileApprovalDecision(conversationID string, requestID any, decision any, targetClientID string, timeout time.Duration) (any, error)
	PermissionsApprovalResponse(conversationID string, requestID any, response map[string]any, targetClientID string, timeout time.Duration) (any, error)
	SubmitUserInput(conversationID string, requestID any, response map[string]any, targetClientID string, timeout time.Duration) (any, error)
	SubmitMcpElicitationResponse(conversationID string, requestID any, response map[string]any, targetClientID string, timeout time.Duration) (any, error)
}

// --- approval store (app-server sourced) ------------------------------------

func canonicalRequestKey(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case json.Number:
		return n.String()
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// typedCodexRequestKey preserves the JSON-RPC id type. Codex may legally use
// both numeric 7 and string "7" on one connection; collapsing them can resolve
// or answer the wrong server request.
func typedCodexRequestKey(v any) string {
	switch v.(type) {
	case string:
		return "s:" + canonicalRequestKey(v)
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return "n:" + canonicalRequestKey(v)
	case nil:
		return ""
	default:
		return "j:" + canonicalRequestKey(v)
	}
}

func codexPendingByDisplayID(pending []*codexPendingApproval, requestID string) (*codexPendingApproval, bool) {
	var match *codexPendingApproval
	for _, req := range pending {
		if req == nil || req.Key != requestID {
			continue
		}
		if match != nil {
			return nil, true
		}
		match = req
	}
	return match, false
}

// codexRequestThreadID correlates old- and new-protocol identifier spellings.
func codexRequestThreadID(params map[string]any) string {
	return firstNonEmpty(
		firstNonEmpty(stringAny(params["threadId"]), stringAny(params["thread_id"])),
		firstNonEmpty(stringAny(params["conversationId"]), stringAny(params["conversation_id"])),
	)
}

func newCodexPendingApproval(requestID any, method string, params map[string]any, source string) *codexPendingApproval {
	return &codexPendingApproval{
		RequestID:  requestID,
		Key:        canonicalRequestKey(requestID),
		RouteKey:   typedCodexRequestKey(requestID),
		Method:     method,
		Params:     params,
		ThreadID:   codexRequestThreadID(params),
		TurnID:     firstNonEmpty(stringAny(params["turnId"]), stringAny(params["turn_id"])),
		ItemID:     firstNonEmpty(firstNonEmpty(stringAny(params["itemId"]), stringAny(params["item_id"])), stringAny(params["callId"])),
		ApprovalID: firstNonEmpty(stringAny(params["approvalId"]), stringAny(params["approval_id"])),
		Source:     source,
		CreatedAt:  time.Now(),
		Instance:   newUUID(),
	}
}

func (c *Codex) addAppServerApproval(generation uint64, requestID any, method string, params map[string]any) string {
	req := newCodexPendingApproval(requestID, method, params, codexApprovalSourceAppServer)
	req.ConnectionGeneration = generation
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	c.approvalsByThread[req.ThreadID] = append(c.approvalsByThread[req.ThreadID], req)
	return req.ThreadID
}

func (c *Codex) removeAppServerApproval(threadID string, routeKey string) *codexPendingApproval {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	remove := func(tid string) *codexPendingApproval {
		list := c.approvalsByThread[tid]
		for i, req := range list {
			if req.RouteKey == routeKey {
				c.approvalsByThread[tid] = append(list[:i], list[i+1:]...)
				if len(c.approvalsByThread[tid]) == 0 {
					delete(c.approvalsByThread, tid)
				}
				return req
			}
		}
		return nil
	}
	if threadID != "" {
		if req := remove(threadID); req != nil {
			return req
		}
		return nil
	}
	for tid := range c.approvalsByThread {
		if req := remove(tid); req != nil {
			return req
		}
	}
	return nil
}

// clearAppServerApprovalsForThread drops pending approvals for one thread
// only — other threads' approvals must survive.
func (c *Codex) clearAppServerApprovalsForThread(threadID string) {
	if threadID == "" {
		return
	}
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	delete(c.approvalsByThread, threadID)
}

func (c *Codex) clearAllAppServerApprovals() {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	c.approvalsByThread = map[string][]*codexPendingApproval{}
}

func (c *Codex) appServerApprovalThreadIDs() []string {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	out := make([]string, 0, len(c.approvalsByThread))
	for threadID, pending := range c.approvalsByThread {
		if threadID != "" && len(pending) > 0 {
			out = append(out, threadID)
		}
	}
	sort.Strings(out)
	return out
}

func (c *Codex) anyAppServerApprovals() bool {
	interrupting := c.interruptingThreadSet()
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	for _, list := range c.approvalsByThread {
		for _, req := range list {
			if !req.Responding && !interrupting[req.ThreadID] {
				return true
			}
		}
	}
	return false
}

// pendingApprovalsForSession returns approvals only from the session's bound
// owner route. With an empty session id it reports app-server approvals across
// all threads for the provider-global view.
func (c *Codex) pendingApprovalsForSession(sessionID string) (string, []*codexPendingApproval) {
	threadID := ""
	out := []*codexPendingApproval{}
	seen := map[string]bool{}
	interrupting := c.interruptingThreadSet()
	route := ""
	if sessionID != "" {
		threadID = c.threadForSession(sessionID)
		route = c.sessionRoute(sessionID)
		if route == "unavailable" {
			return threadID, out
		}
	}
	desktopRoute := route == "desktop_ipc"
	c.approvalMu.Lock()
	if sessionID == "" {
		for _, list := range c.approvalsByThread {
			for _, req := range list {
				if !req.Responding && !interrupting[req.ThreadID] {
					out = append(out, req)
				}
			}
		}
	} else if !desktopRoute {
		if !interrupting[threadID] {
			for _, req := range c.approvalsByThread[threadID] {
				if !req.Responding {
					out = append(out, req)
				}
			}
		}
	}
	c.approvalMu.Unlock()
	for _, req := range out {
		seen[req.Key] = true
	}
	if threadID != "" && desktopRoute {
		if b := c.desktopBridge(); b != nil {
			for _, breq := range b.PendingHumanRequests(threadID) {
				req := newCodexPendingApproval(breq.ID, breq.Method, breq.Params, codexApprovalSourceDesktop)
				req.Instance = breq.Instance
				if req.ThreadID == "" {
					req.ThreadID = threadID
				}
				if seen[req.Key] {
					continue
				}
				seen[req.Key] = true
				if ms := int64Any(breq.Params["startedAtMs"]); ms > 0 {
					req.CreatedAt = time.UnixMilli(ms)
				}
				out = append(out, req)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return threadID, out
}

// --- approval presentation ----------------------------------------------------

func codexApprovalKind(method string) string {
	switch {
	case strings.Contains(method, "commandExecution") || strings.Contains(method, "execCommand"):
		return "command"
	case strings.Contains(method, "fileChange") || strings.Contains(method, "applyPatch"):
		return "file_change"
	case strings.Contains(method, "permissions"):
		return "permissions"
	case strings.Contains(method, "requestUserInput"):
		return "question"
	case strings.Contains(method, "elicitation"):
		return "elicitation"
	}
	return "operation"
}

func codexApprovalView(req *codexPendingApproval) map[string]any {
	kind := codexApprovalKind(req.Method)
	out := map[string]any{
		"type":             kind,
		"request_id":       req.Key,
		"thread_id":        req.ThreadID,
		"method":           req.Method,
		"source":           req.Source,
		"request_instance": req.Instance,
	}
	if req.Source == codexApprovalSourceDesktop && !codexDesktopApprovalMethodSupported(req.Method) {
		// The Desktop follower IPC has method-specific response routes. Keep
		// forward-compatible requests visible, but do not show an allow/deny
		// button that cannot safely reach the owning Desktop client.
		out["actionable"] = false
	}
	if req.TurnID != "" {
		out["turn_id"] = req.TurnID
	}
	if req.ItemID != "" {
		out["item_id"] = req.ItemID
	}
	if req.ApprovalID != "" {
		out["approval_id"] = req.ApprovalID
	}
	if req.Source != codexApprovalSourceDesktop || int64Any(req.Params["startedAtMs"]) > 0 {
		out["created_at"] = req.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if decisions := codexStringDecisions(req.Params); len(decisions) > 0 {
		out["decisions"] = decisions
	}
	if kind == "question" {
		questions := []map[string]any{}
		for _, raw := range listAny(req.Params["questions"]) {
			q := mapAny(raw)
			options := []map[string]any{}
			for _, optRaw := range listAny(q["options"]) {
				opt := mapAny(optRaw)
				options = append(options, map[string]any{
					"label":       stringAny(opt["label"]),
					"description": stringAny(opt["description"]),
				})
			}
			questions = append(questions, map[string]any{
				"id":          stringAny(q["id"]),
				"header":      stringAny(q["header"]),
				"question":    stringAny(q["question"]),
				"options":     options,
				"multiSelect": false,
			})
		}
		out["questions"] = questions
		out["summary"] = "codex requests input"
		return out
	}
	reason := stringAny(req.Params["reason"])
	if reason == "" && kind == "elicitation" {
		reason = stringAny(req.Params["message"])
	}
	cmd := commandString(req.Params["command"])
	summary := "codex requests approval: " + kind
	if reason != "" {
		summary += " - " + reason
	}
	raw := mustJSON(req.Params)
	if len(raw) > 12000 {
		raw = raw[:12000] + "\n... (truncated)"
	}
	details := raw
	if cmd != "" {
		details = "$ " + cmd + "\n\n" + raw
	}
	out["summary"] = summary
	out["details"] = details
	return out
}

func codexDesktopApprovalMethodSupported(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
		"item/permissions/requestApproval", "item/tool/requestUserInput", "mcpServer/elicitation/request":
		return true
	}
	return false
}

func codexStringDecisions(params map[string]any) []string {
	out := []string{}
	for _, raw := range listAny(params["availableDecisions"]) {
		if s, ok := raw.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// codexCommandDecisionValue picks the wire decision value for command /
// file-change approvals, honouring availableDecisions when the server
// constrains what may be presented.
func codexCommandDecisionValue(allow bool, params map[string]any) (string, error) {
	decisions := codexStringDecisions(params)
	has := func(v string) bool {
		if len(decisions) == 0 {
			return true // no constraint advertised
		}
		for _, d := range decisions {
			if d == v {
				return true
			}
		}
		return false
	}
	if allow {
		if has("accept") {
			return "accept", nil
		}
		return "", errors.New("approval does not offer an accept decision")
	}
	if has("decline") {
		return "decline", nil
	}
	if has("cancel") {
		return "cancel", nil
	}
	return "", errors.New("approval offers neither decline nor cancel")
}

// codexApprovalResponseBody builds the correct app-server response payload
// for each protocol generation. allow and deny must differ for every method.
func codexApprovalResponseBody(method string, allow bool, params map[string]any) (map[string]any, error) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		value, err := codexCommandDecisionValue(allow, params)
		if err != nil {
			return nil, err
		}
		return map[string]any{"decision": value}, nil
	case "item/permissions/requestApproval":
		if allow {
			granted := mapAny(deepCopyJSON(mapAny(params["permissions"])))
			return map[string]any{"permissions": granted, "scope": "turn"}, nil
		}
		return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
	case "execCommandApproval", "applyPatchApproval":
		if allow {
			return map[string]any{"decision": "approved"}, nil
		}
		return map[string]any{"decision": "denied"}, nil
	case "mcpServer/elicitation/request":
		if allow {
			return map[string]any{"action": "accept", "content": map[string]any{}}, nil
		}
		return map[string]any{"action": "decline"}, nil
	}
	// Forward-compatible app-server approval methods use the modern accept /
	// decline decision vocabulary. Surfacing the request but then being unable
	// to answer it is worse than preserving this protocol convention.
	if strings.HasSuffix(method, "/requestApproval") {
		return map[string]any{"decision": map[bool]string{true: "accept", false: "decline"}[allow]}, nil
	}
	if strings.HasSuffix(method, "Approval") {
		return map[string]any{"decision": map[bool]string{true: "approved", false: "denied"}[allow]}, nil
	}
	return nil, errors.New("unsupported approval method: " + method)
}

// codexUserInputResponseBody maps web answers (keyed by question id, question
// text, or index) into a ToolRequestUserInputResponse body.
func codexUserInputResponseBody(params map[string]any, answers map[string]string) (map[string]any, error) {
	questions := listAny(params["questions"])
	if len(questions) == 0 {
		return nil, errors.New("question request has no questions")
	}
	out := map[string]any{}
	for idx, raw := range questions {
		q := mapAny(raw)
		qid := stringAny(q["id"])
		text := stringAny(q["question"])
		value, ok := answers[qid]
		if !ok {
			value, ok = answers[text]
		}
		if !ok {
			value, ok = answers[strconv.Itoa(idx)]
		}
		if !ok || strings.TrimSpace(value) == "" {
			return nil, errors.New("missing answer for question " + firstNonEmpty(qid, strconv.Itoa(idx)))
		}
		key := qid
		if key == "" {
			key = strconv.Itoa(idx)
		}
		out[key] = map[string]any{"answers": []any{value}}
	}
	return map[string]any{"answers": out}, nil
}

func workspaceDependenciesText() string {
	base := "~/.cache/codex-runtimes/codex-primary-runtime/dependencies"
	return strings.Join([]string{
		"Workspace dependencies are available for this local desktop thread.",
		"",
		"### Workspace Dependencies",
		"Use these bundled paths for sheets, slides, documents, PDFs, images, or browser automation:",
		"- Node.js executable: " + base + "/node/bin/node",
		"- Node.js packages: " + base + "/node/node_modules",
		"- Python executable: " + base + "/python/bin/python3",
		"- Python packages: " + base + "/python",
		"- Native binaries: " + base + "/bin",
	}, "\n")
}

func (c *Codex) currentClientModel() string {
	c.clientMu.Lock()
	client := c.client
	c.clientMu.Unlock()
	if client == nil {
		return ""
	}
	return client.LastModel()
}

func modelOptionsFromExtra(raw any) []ModelOption {
	out := []ModelOption{}
	for _, item := range listAny(raw) {
		switch v := item.(type) {
		case string:
			out = append(out, ModelOption{ID: v, Label: v})
		case map[string]any:
			id := stringAny(v["id"])
			if id != "" {
				out = append(out, ModelOption{ID: id, Label: firstNonEmpty(stringAny(v["label"]), id)})
			}
		}
	}
	return out
}

func modeOptionsFromExtra(raw any, fallback []ModeOption) []ModeOption {
	out := []ModeOption{}
	for _, item := range listAny(raw) {
		if m := mapAny(item); len(m) > 0 {
			id := stringAny(m["id"])
			if id != "" {
				out = append(out, ModeOption{ID: id, Label: firstNonEmpty(stringAny(m["label"]), id)})
			}
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func stringPtrIf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
