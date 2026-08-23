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
	"strings"
	"sync"
	"time"

	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/turnstatehook"
)

const (
	claudeRouteDesktopComputerUse = "desktop_computer_use"
	claudeRouteStreamJSONCLI      = "stream_json_cli"

	claudeDesktopDefaultBundleID = "com.anthropic.claudefordesktop"
	claudeDesktopDefaultTeamID   = "Q6L2SF6YDW"
	claudeDesktopDefaultAppPath  = "/Applications/Claude.app"

	// A real Claude.app deep verification can take more than two seconds on
	// Apple Silicon. Keep the readiness probe bounded, but leave enough room for
	// codesign to validate the full bundle on an ordinarily loaded host.
	claudeDesktopReadinessTimeout    = 5 * time.Second
	claudeDesktopReadinessSuccessTTL = 15 * time.Second
	claudeDesktopReadinessFailureTTL = time.Second
)

type claudeDesktopReadinessKey struct {
	appPath  string
	bundleID string
	teamID   string
}

type claudeDesktopReadinessResult struct {
	key       claudeDesktopReadinessKey
	ready     bool
	expiresAt time.Time
}

type claudeDesktopReadinessCheck struct {
	key   claudeDesktopReadinessKey
	epoch uint64
	done  chan struct{}
}

func (c *Claude) claudePrimaryRoute() string {
	if stringExtra(c.cfg.Extra, "primary_route", claudeRouteDesktopComputerUse) == claudeRouteStreamJSONCLI {
		return claudeRouteStreamJSONCLI
	}
	return claudeRouteDesktopComputerUse
}

func (c *Claude) claudeFallbackRoute() string {
	if stringExtra(c.cfg.Extra, "fallback_route", claudeRouteStreamJSONCLI) == claudeRouteStreamJSONCLI {
		return claudeRouteStreamJSONCLI
	}
	return ""
}

func (c *Claude) claudeUIOperationTimeout() time.Duration {
	timeout := durationExtra(c.cfg.Extra, "ui_operation_timeout_seconds", 30*time.Second)
	if timeout < time.Second {
		return time.Second
	}
	if timeout > 2*time.Minute {
		return 2 * time.Minute
	}
	return timeout
}

func (c *Claude) transcriptIDForDesktopRoute(sessionID string) string {
	transcriptID := c.transcriptID(sessionID)
	if !claudeUUIDRE.MatchString(transcriptID) {
		return ""
	}
	return transcriptID
}

func (c *Claude) claudeDesktopAppVerified(ctx context.Context) bool {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	verify := runtime.deps.verifyApp
	runtime.mu.Unlock()
	if verify == nil {
		return false
	}
	key := claudeDesktopReadinessKey{
		appPath:  stringExtra(c.cfg.Extra, "desktop_app_path", claudeDesktopDefaultAppPath),
		bundleID: stringExtra(c.cfg.Extra, "desktop_bundle_id", claudeDesktopDefaultBundleID),
		teamID:   stringExtra(c.cfg.Extra, "desktop_team_id", claudeDesktopDefaultTeamID),
	}

	for {
		now := time.Now()
		c.desktopReadinessMu.Lock()
		if c.desktopReadiness.key == key && now.Before(c.desktopReadiness.expiresAt) {
			ready := c.desktopReadiness.ready
			c.desktopReadinessMu.Unlock()
			return ready
		}
		if check := c.desktopReadinessCheck; check != nil {
			done := check.done
			c.desktopReadinessMu.Unlock()
			select {
			case <-ctx.Done():
				return false
			case <-done:
				continue
			}
		}
		check := &claudeDesktopReadinessCheck{
			key: key, epoch: c.desktopReadinessEpoch, done: make(chan struct{}),
		}
		c.desktopReadinessCheck = check
		c.desktopReadinessMu.Unlock()

		ready := verify(ctx, key.appPath, key.bundleID, key.teamID) == nil
		ttl := claudeDesktopReadinessFailureTTL
		if ready {
			ttl = claudeDesktopReadinessSuccessTTL
		}
		c.desktopReadinessMu.Lock()
		if c.desktopReadinessCheck == check {
			// This cache is only a readiness/status optimization. Every prompt or
			// permission operation still calls verifyApp directly immediately
			// before launching or touching Claude, so a cached result never grants
			// authority to mutate the application UI.
			if check.epoch == c.desktopReadinessEpoch {
				c.desktopReadiness = claudeDesktopReadinessResult{
					key: key, ready: ready, expiresAt: time.Now().Add(ttl),
				}
			}
			c.desktopReadinessCheck = nil
			close(check.done)
		}
		c.desktopReadinessMu.Unlock()
		return ready
	}
}

func (c *Claude) invalidateClaudeDesktopReadiness() {
	c.desktopReadinessMu.Lock()
	c.desktopReadinessEpoch++
	c.desktopReadiness = claudeDesktopReadinessResult{}
	c.desktopReadinessMu.Unlock()
}

type claudeComputerUseDisposition string

const (
	// No application input or button mutation was attempted. A brand-new,
	// unbound session may safely choose the CLI fallback exactly once.
	claudeComputerUseNotAttempted claudeComputerUseDisposition = "not_attempted"
	// The exact Claude surface acknowledged the mutation and its UI reached the
	// expected postcondition.
	claudeComputerUseConfirmed claudeComputerUseDisposition = "confirmed"
	// An application mutation was attempted but its postcondition is unknown.
	// Retrying through the CLI could duplicate a prompt or permission decision.
	claudeComputerUseDeliveryUnknown claudeComputerUseDisposition = "delivery_unknown"
)

type claudeComputerUseOutcome struct {
	Disposition      claudeComputerUseDisposition
	FallbackAllowed  bool
	DesktopSessionID string
	TranscriptID     string
	Err              error
}

type claudePromptAttemptSpec struct {
	operationID string
	prompt      string
	attachments []Attachment
}

func (o claudeComputerUseOutcome) canFallback() bool {
	return o.Disposition == claudeComputerUseNotAttempted && o.FallbackAllowed
}

var errClaudeComputerUseUnavailable = errors.New("Claude Desktop computer use is unavailable")

type claudeComputerUseDependencies struct {
	verifyApp         func(context.Context, string, string, string) error
	launchApp         func(context.Context, string) error
	waitApp           func(context.Context, string) error
	openURL           func(context.Context, string, string) error
	sessions          func() []map[string]any
	messages          func(string) []map[string]any
	records           func(string) []map[string]any
	interactions      func() ([]turnstatehook.InteractionRecord, error)
	removeInteraction func(string) error
	sleep             func(context.Context, time.Duration) error
	now               func() time.Time
}

type claudeComputerUseRuntime struct {
	mu           sync.Mutex
	handler      ComputerUseAutomationHandler
	readiness    ComputerUseReadinessHandler
	commitRoute  ClaudeControlRouteCommitHandler
	deps         claudeComputerUseDependencies
	routes       map[string]claudeComputerUseRouteBinding
	active       map[string]bool
	startOptions map[string]StartOptions
}

type claudeComputerUseRouteBinding struct {
	route     string
	committed bool
}

var claudeComputerUseRuntimes sync.Map // map[*Claude]*claudeComputerUseRuntime

func claudeComputerUseRuntimeFor(c *Claude) *claudeComputerUseRuntime {
	if c == nil {
		return &claudeComputerUseRuntime{
			routes: map[string]claudeComputerUseRouteBinding{}, active: map[string]bool{}, startOptions: map[string]StartOptions{},
		}
	}
	if raw, ok := claudeComputerUseRuntimes.Load(c); ok {
		return raw.(*claudeComputerUseRuntime)
	}
	runtime := &claudeComputerUseRuntime{
		routes: map[string]claudeComputerUseRouteBinding{}, active: map[string]bool{},
		startOptions: map[string]StartOptions{},
	}
	runtime.deps = claudeComputerUseDependencies{
		verifyApp: claudeVerifyDesktopApp,
		launchApp: func(ctx context.Context, appPath string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", "-gj", "-a", appPath).Run()
		},
		waitApp: claudeWaitForDesktopProcess,
		openURL: func(ctx context.Context, appPath, rawURL string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", "-gj", "-a", appPath, rawURL).Run()
		},
		sessions: func() []map[string]any {
			return claudeDesktopSessions(stringExtra(c.cfg.Extra, "claude_code_sessions_dir", ""), 0)
		},
		messages: func(transcriptID string) []map[string]any {
			return claudeSessionMessages(
				transcriptID, stringExtra(c.cfg.Extra, "claude_projects_dir", ""), nativePreviewUnlimited,
			)
		},
		records: func(transcriptID string) []map[string]any {
			path := claudeTranscriptPath(transcriptID, stringExtra(c.cfg.Extra, "claude_projects_dir", ""))
			if path == "" {
				return nil
			}
			return jsonlTailRecords(path, jsonlTailScanBytes)
		},
		interactions: func() ([]turnstatehook.InteractionRecord, error) {
			return turnstatehook.ListInteractions(
				stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"),
				turnstatehook.DefaultInteractionTTL,
			)
		},
		removeInteraction: func(requestID string) error {
			return turnstatehook.RemoveInteraction(
				stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"), requestID,
			)
		},
		sleep: claudeComputerUseSleep,
		now:   time.Now,
	}
	actual, _ := claudeComputerUseRuntimes.LoadOrStore(c, runtime)
	return actual.(*claudeComputerUseRuntime)
}

func (c *Claude) rememberClaudeStartOptions(sessionID string, opts StartOptions) {
	if sessionID == "" {
		return
	}
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	runtime.startOptions[sessionID] = opts
	runtime.mu.Unlock()
}

// BindClaudeControlStartOptions restores the complete create-time contract
// from the durable API session record. Keeping this separate from route
// hydration avoids silently losing mode/model/effort across a provider restart.
func (c *Claude) BindClaudeControlStartOptions(sessionID string, opts StartOptions) {
	c.rememberClaudeStartOptions(sessionID, opts)
}

func (c *Claude) claudeStartOptions(sessionID string) StartOptions {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.startOptions[sessionID]
}

func claudeComputerUseSleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SetComputerUseAutomationHandler implements ComputerUseAutomationHost. The
// callback is stored out-of-line so the Claude struct can keep CLI runtime
// state independent from the short-lived desktop authority seam.
func (c *Claude) SetComputerUseAutomationHandler(handler ComputerUseAutomationHandler) {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	runtime.handler = handler
	runtime.mu.Unlock()
	c.invalidateClaudeDesktopReadiness()
}

// SetComputerUseReadinessHandler installs a status-only view of the signed
// desktop helper. Operations never use this snapshot as authority: every
// prompt, answer, and permission decision still passes the helper's live gates.
func (c *Claude) SetComputerUseReadinessHandler(handler ComputerUseReadinessHandler) {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	runtime.readiness = handler
	runtime.mu.Unlock()
}

func (c *Claude) claudeComputerUseReadiness(ctx context.Context) (ComputerUseReadiness, bool) {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	automation := runtime.handler
	readiness := runtime.readiness
	runtime.mu.Unlock()
	if automation == nil || readiness == nil || ctx == nil || ctx.Err() != nil {
		return ComputerUseReadiness{}, false
	}
	return readiness(ctx), true
}

func (c *Claude) SetClaudeControlRouteCommitHandler(handler ClaudeControlRouteCommitHandler) {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	runtime.commitRoute = handler
	runtime.mu.Unlock()
}

func (c *Claude) commitClaudeControlRoute(ctx context.Context, sessionID, route string) error {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	handler := runtime.commitRoute
	runtime.mu.Unlock()
	if handler == nil {
		return errors.New("Claude control-route durability barrier is unavailable")
	}
	if err := handler(ctx, sessionID, route); err != nil {
		return err
	}
	if !c.bindClaudeComputerUseRouteState(sessionID, route, true) {
		return errors.New("Claude control route conflicts with its committed owner")
	}
	return nil
}

func (c *Claude) claudeComputerUseSetDependencies(deps claudeComputerUseDependencies) func() {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	old := runtime.deps
	if deps.verifyApp != nil {
		runtime.deps.verifyApp = deps.verifyApp
	}
	if deps.launchApp != nil {
		runtime.deps.launchApp = deps.launchApp
	}
	if deps.waitApp != nil {
		runtime.deps.waitApp = deps.waitApp
	}
	if deps.openURL != nil {
		runtime.deps.openURL = deps.openURL
	}
	if deps.sessions != nil {
		runtime.deps.sessions = deps.sessions
	}
	if deps.messages != nil {
		runtime.deps.messages = deps.messages
	}
	if deps.records != nil {
		runtime.deps.records = deps.records
	}
	if deps.interactions != nil {
		runtime.deps.interactions = deps.interactions
	}
	if deps.removeInteraction != nil {
		runtime.deps.removeInteraction = deps.removeInteraction
	}
	if deps.sleep != nil {
		runtime.deps.sleep = deps.sleep
	}
	if deps.now != nil {
		runtime.deps.now = deps.now
	}
	runtime.mu.Unlock()
	c.invalidateClaudeDesktopReadiness()
	return func() {
		runtime.mu.Lock()
		runtime.deps = old
		runtime.mu.Unlock()
		c.invalidateClaudeDesktopReadiness()
	}
}

func claudeVerifyDesktopApp(ctx context.Context, appPath, bundleID, teamID string) error {
	info, err := os.Stat(appPath)
	if err != nil || !info.IsDir() {
		return errors.New("Claude Desktop application is unavailable")
	}
	if err := exec.CommandContext(
		ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", appPath,
	).Run(); err != nil {
		return errors.New("Claude Desktop code signature is invalid")
	}
	out, err := exec.CommandContext(
		ctx, "/usr/bin/codesign", "-d", "--verbose=4", appPath,
	).CombinedOutput()
	if err != nil || len(out) > 64*1024 {
		return errors.New("Claude Desktop signing identity is unavailable")
	}
	identifier, team := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		if value, ok := strings.CutPrefix(line, "Identifier="); ok {
			identifier = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "TeamIdentifier="); ok {
			team = strings.TrimSpace(value)
		}
	}
	if identifier != bundleID || team != teamID {
		return errors.New("Claude Desktop signing identity does not match configuration")
	}
	return nil
}

func claudeWaitForDesktopProcess(ctx context.Context, appPath string) error {
	executablePrefix := filepath.Join(appPath, "Contents", "MacOS") + string(os.PathSeparator)
	for {
		out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "command=").Output()
		if err == nil && len(out) <= 4*1024*1024 {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), executablePrefix) {
					return nil
				}
			}
		}
		if err := claudeComputerUseSleep(ctx, 100*time.Millisecond); err != nil {
			return errors.New("Claude Desktop process did not become ready")
		}
	}
}

func (c *Claude) claudeComputerUseRoute(sessionID string) string {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.routes[sessionID].route
}

func (c *Claude) ClaudeControlRoute(sessionID string) string {
	if route := c.claudeComputerUseRoute(sessionID); route != "" {
		return route
	}
	return c.claudePrimaryRoute()
}

func (c *Claude) ClaudeControlRouteCommitted(sessionID string) bool {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.routes[sessionID].committed
}

func (c *Claude) BindClaudeControlCommitted(sessionID string, committed bool) {
	if sessionID == "" {
		return
	}
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	binding := runtime.routes[sessionID]
	if binding.route != "" {
		binding.committed = binding.committed || committed
		runtime.routes[sessionID] = binding
	}
	runtime.mu.Unlock()
}

func (c *Claude) BindClaudeControlRoute(sessionID, transcriptID, route, cwd string) {
	if sessionID == "" {
		return
	}
	if strings.TrimSpace(cwd) != "" {
		opts := c.claudeStartOptions(sessionID)
		opts.Cwd = cwd
		c.rememberClaudeStartOptions(sessionID, opts)
	}
	if transcriptID != "" {
		c.BindTranscript(sessionID, transcriptID)
	}
	committed := transcriptID != "" || route == claudeRouteStreamJSONCLI
	c.bindClaudeComputerUseRouteState(sessionID, route, committed)
}

func (c *Claude) bindClaudeComputerUseRoute(sessionID, route string) bool {
	return c.bindClaudeComputerUseRouteState(sessionID, route, true)
}

func (c *Claude) bindClaudeComputerUseRouteState(sessionID, route string, committed bool) bool {
	if sessionID == "" || (route != claudeRouteDesktopComputerUse && route != claudeRouteStreamJSONCLI) {
		return false
	}
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	current := runtime.routes[sessionID]
	if current.route != "" && current.route != route &&
		(current.committed || current.route != claudeRouteDesktopComputerUse || route != claudeRouteStreamJSONCLI) {
		return false
	}
	runtime.routes[sessionID] = claudeComputerUseRouteBinding{route: route, committed: current.committed || committed}
	return true
}

func (c *Claude) tentativelyBindClaudeComputerUseRoute(sessionID string) bool {
	return c.bindClaudeComputerUseRouteState(sessionID, claudeRouteDesktopComputerUse, false)
}

func (c *Claude) commitClaudeCLIFallbackRoute(sessionID string) bool {
	return c.bindClaudeComputerUseRouteState(sessionID, claudeRouteStreamJSONCLI, true)
}

func (c *Claude) beginClaudeComputerUse(sessionID string) (
	ComputerUseAutomationHandler, claudeComputerUseDependencies, func(), error,
) {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	if runtime.handler == nil {
		runtime.mu.Unlock()
		return nil, claudeComputerUseDependencies{}, nil, errClaudeComputerUseUnavailable
	}
	if runtime.routes[sessionID].route == claudeRouteStreamJSONCLI {
		runtime.mu.Unlock()
		return nil, claudeComputerUseDependencies{}, nil, errors.New("Claude session is bound to the CLI route")
	}
	if runtime.active[sessionID] {
		runtime.mu.Unlock()
		return nil, claudeComputerUseDependencies{}, nil, errors.New("Claude Desktop operation is already active")
	}
	runtime.active[sessionID] = true
	handler, deps := runtime.handler, runtime.deps
	runtime.mu.Unlock()
	return handler, deps, func() {
		runtime.mu.Lock()
		delete(runtime.active, sessionID)
		runtime.mu.Unlock()
	}, nil
}

type claudeDesktopTarget struct {
	logicalID   string
	desktopID   string
	cliID       string
	title       string
	titleUnique bool
	updatedAt   string
	cwd         string
	originCwd   string
	mode        string
	model       string
	requested   StartOptions
	new         bool
	baseline    map[string]bool
}

func (c *Claude) claudeDesktopTarget(sessionID string, rows []map[string]any) (claudeDesktopTarget, error) {
	aliases := map[string]bool{strings.TrimSpace(sessionID): true}
	if transcriptID := strings.TrimSpace(c.transcriptID(sessionID)); transcriptID != "" {
		aliases[transcriptID] = true
	}
	target := claudeDesktopTarget{
		logicalID: sessionID, requested: c.claudeStartOptions(sessionID), baseline: claudeDesktopSessionKeys(rows),
	}
	matches := make([]claudeDesktopTarget, 0, 1)
	seen := map[string]bool{}
	for _, row := range rows {
		cliID := strings.TrimSpace(stringAny(row["cli_session_id"]))
		desktopID := strings.TrimSpace(firstNonEmpty(stringAny(row["desktop_session_id"]), stringAny(row["native_session_id"])))
		if !aliases[cliID] && !aliases[desktopID] {
			continue
		}
		key := cliID + "\x00" + desktopID
		if seen[key] {
			continue
		}
		seen[key] = true
		matches = append(matches, claudeDesktopTarget{
			logicalID: sessionID, cliID: cliID, desktopID: desktopID,
			title: strings.TrimSpace(stringAny(row["title"])), updatedAt: stringAny(row["updated_at"]),
			cwd: stringAny(row["cwd"]), originCwd: stringAny(row["origin_cwd"]),
			mode: stringAny(row["permission_mode"]), model: stringAny(row["model"]),
			requested: target.requested, baseline: target.baseline,
		})
	}
	if len(matches) > 1 {
		return target, errors.New("Claude Desktop session identity is ambiguous")
	}
	if len(matches) == 1 {
		target = matches[0]
		if !claudeUUIDRE.MatchString(target.cliID) {
			return target, errors.New("Claude Desktop session has no exact transcript UUID")
		}
		if target.title == "" || target.title == "(untitled)" {
			return target, errors.New("Claude Desktop session has no verifiable title")
		}
		target.titleUnique = claudeDesktopTitleCount(rows, target.title) == 1
		if !target.titleUnique && target.desktopID == "" {
			return target, errors.New("Claude Desktop session title is not unique")
		}
		return target, nil
	}
	for alias := range aliases {
		if claudeUUIDRE.MatchString(alias) {
			return target, errors.New("Claude transcript is not bound to an exact Desktop session")
		}
	}
	target.new = true
	return target, nil
}

func claudeDesktopTitleCount(rows []map[string]any, title string) int {
	want := claudeAXText(title)
	count := 0
	for _, row := range rows {
		if claudeAXText(stringAny(row["title"])) == want {
			count++
		}
	}
	return count
}

func claudeDesktopSessionKeys(rows []map[string]any) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		key := strings.TrimSpace(firstNonEmpty(
			stringAny(row["desktop_session_id"]), stringAny(row["native_session_id"]),
		))
		if key == "" {
			key = strings.TrimSpace(stringAny(row["cli_session_id"]))
		}
		if key != "" {
			out[key] = true
		}
	}
	return out
}

func claudeInteractionTranscriptID(record turnstatehook.InteractionRecord) string {
	if record.TranscriptPath == "" {
		return ""
	}
	base := filepath.Base(record.TranscriptPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func claudeCanonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func claudePendingObservedTool(
	transcriptPath, toolUseID, toolName string, toolInput any,
) bool {
	wantInput, err := claudeCanonicalJSON(toolInput)
	if err != nil || toolUseID == "" || toolName == "" {
		return false
	}
	found := 0
	resolved := false
	for _, record := range jsonlTailRecords(transcriptPath, jsonlTailScanBytes) {
		content := mapAny(record["message"])["content"]
		switch stringAny(record["type"]) {
		case "assistant":
			for _, raw := range listAny(content) {
				block := mapAny(raw)
				if stringAny(block["type"]) != "tool_use" || stringAny(block["id"]) != toolUseID ||
					stringAny(block["name"]) != toolName {
					continue
				}
				gotInput, marshalErr := claudeCanonicalJSON(block["input"])
				if marshalErr != nil || string(gotInput) != string(wantInput) {
					return false
				}
				found++
			}
		case "user":
			for _, raw := range listAny(content) {
				block := mapAny(raw)
				if stringAny(block["type"]) == "tool_result" && stringAny(block["tool_use_id"]) == toolUseID {
					resolved = true
				}
			}
		}
	}
	return found == 1 && !resolved
}

func (c *Claude) claudeDesktopInteractionRaw(
	sessionID, requestID, toolName string,
) (turnstatehook.InteractionRecord, error) {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	list := runtime.deps.interactions
	runtime.mu.Unlock()
	if list == nil {
		return turnstatehook.InteractionRecord{}, errors.New("Claude interaction observer is unavailable")
	}
	records, err := list()
	if err != nil {
		return turnstatehook.InteractionRecord{}, err
	}
	transcriptID := c.transcriptIDForDesktopRoute(sessionID)
	if transcriptID == "" {
		return turnstatehook.InteractionRecord{}, errors.New("Claude Desktop interaction has no bound transcript")
	}
	expectedPath := claudeTranscriptPath(transcriptID, stringExtra(c.cfg.Extra, "claude_projects_dir", ""))
	expectedReal, expectedErr := filepath.EvalSymlinks(expectedPath)
	if expectedPath == "" || expectedErr != nil {
		return turnstatehook.InteractionRecord{}, errors.New("Claude interaction transcript is unavailable")
	}
	matches := make([]turnstatehook.InteractionRecord, 0, 2)
	for _, record := range records {
		recordReal, realErr := filepath.EvalSymlinks(record.TranscriptPath)
		if realErr != nil || recordReal != expectedReal || record.SessionID != transcriptID ||
			claudeInteractionTranscriptID(record) != transcriptID {
			continue
		}
		if record.RequestID == "" || record.ToolUseID == "" || record.ToolName == "" {
			continue
		}
		wantEvent := "PermissionRequest"
		if record.ToolName == "AskUserQuestion" {
			wantEvent = "PreToolUse"
		}
		if record.HookEventName != wantEvent ||
			!claudePendingObservedTool(expectedReal, record.ToolUseID, record.ToolName, record.ToolInput) {
			continue
		}
		matches = append(matches, record)
	}
	if len(matches) == 1 {
		record := matches[0]
		if requestID != "" && (record.RequestID != requestID || record.ToolUseID != requestID) {
			return turnstatehook.InteractionRecord{}, errors.New("Claude interaction request identity does not match")
		}
		if toolName != "" && record.ToolName != toolName {
			return turnstatehook.InteractionRecord{}, errors.New("Claude interaction tool identity does not match")
		}
		return record, nil
	}
	if len(matches) > 1 {
		// Native permission/question cards do not expose tool_use_id or request_id
		// through Accessibility. Even an API call naming one request cannot safely
		// choose between two simultaneous cards, so fail closed before opening UI.
		return turnstatehook.InteractionRecord{}, errors.New("Claude interaction identity is ambiguous")
	}
	return turnstatehook.InteractionRecord{}, errors.New("Claude interaction is no longer pending")
}

var (
	errClaudeInteractionAttempted = errors.New("Claude Desktop interaction delivery is unknown")
	errClaudeInteractionResolved  = errors.New("Claude Desktop interaction is already resolved")
	errClaudeDesktopTurnRunning   = errors.New("Claude Desktop session turn is still running")
)

func (c *Claude) claudeDesktopInteraction(
	sessionID, requestID, toolName string,
) (turnstatehook.InteractionRecord, error) {
	record, err := c.claudeDesktopInteractionRaw(sessionID, requestID, toolName)
	if err != nil {
		return turnstatehook.InteractionRecord{}, err
	}
	state, err := turnstatehook.InteractionCandidateStateForRecord(
		stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"), record,
	)
	if err != nil {
		return record, fmt.Errorf("classify Claude Desktop interaction: %w", err)
	}
	switch state {
	case turnstatehook.InteractionCandidatePending:
		return record, nil
	case turnstatehook.InteractionCandidateAttempted:
		return record, errClaudeInteractionAttempted
	case turnstatehook.InteractionCandidateResolved:
		return record, errClaudeInteractionResolved
	default:
		return record, errors.New("Claude Desktop interaction has invalid durable state")
	}
}

func claudeDesktopInteractionUnknownRequest(record turnstatehook.InteractionRecord) map[string]any {
	request := claudeDesktopInteractionRequest(record)
	if request == nil {
		return nil
	}
	request["actionable"] = false
	request["state"] = "delivery_unknown"
	request["summary"] = "Claude Desktop interaction may already have been delivered; it will not be retried"
	return request
}

func (c *Claude) removeClaudeDesktopInteraction(requestID string) error {
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	remove := runtime.deps.removeInteraction
	runtime.mu.Unlock()
	if remove == nil {
		return errors.New("Claude interaction observer cleanup is unavailable")
	}
	return remove(requestID)
}

func claudeDesktopInteractionRequest(record turnstatehook.InteractionRecord) map[string]any {
	if record.ToolName == "AskUserQuestion" {
		question := claudeAskUserQuestionRequest(
			record.ToolInput, record.ToolUseID, record.RecordedAt.UTC().Format(time.RFC3339Nano),
		)
		if question == nil {
			return nil
		}
		question["request_id"] = record.RequestID
		question["source"] = "claude_desktop_observer"
		question["actionable"] = true
		return question
	}
	typ := "operation"
	switch strings.ToLower(record.ToolName) {
	case "bash":
		typ = "command"
	case "edit", "write", "notebookedit":
		typ = "file_change"
	}
	return map[string]any{
		"type": typ, "summary": "Claude Desktop requests temporary tool access",
		"details": mustJSON(record.ToolInput), "request_id": record.RequestID,
		"tool_use_id": record.ToolUseID, "tool_name": record.ToolName,
		"source": "claude_desktop_observer", "actionable": true,
	}
}

func claudeNewDesktopSession(
	target *claudeDesktopTarget, rows []map[string]any,
) (bool, error) {
	if target == nil || !target.new {
		return true, nil
	}
	candidates := make([]claudeDesktopTarget, 0, 1)
	seen := map[string]bool{}
	for _, row := range rows {
		desktopID := strings.TrimSpace(firstNonEmpty(
			stringAny(row["desktop_session_id"]), stringAny(row["native_session_id"]),
		))
		cliID := strings.TrimSpace(stringAny(row["cli_session_id"]))
		key := desktopID
		if key == "" {
			key = cliID
		}
		if key == "" || target.baseline[key] || seen[key] {
			continue
		}
		seen[key] = true
		if desktopID == "" || !claudeUUIDRE.MatchString(cliID) {
			continue
		}
		title := strings.TrimSpace(stringAny(row["title"]))
		if title == "" || title == "(untitled)" {
			continue
		}
		candidates = append(candidates, claudeDesktopTarget{
			logicalID: target.logicalID, desktopID: desktopID, cliID: cliID,
			title: title, titleUnique: claudeDesktopTitleCount(rows, title) == 1,
			updatedAt: stringAny(row["updated_at"]), cwd: stringAny(row["cwd"]),
			originCwd: stringAny(row["origin_cwd"]), mode: stringAny(row["permission_mode"]),
			model:     stringAny(row["model"]),
			requested: target.requested, baseline: target.baseline,
		})
	}
	if len(candidates) > 1 {
		return false, errors.New("new Claude Desktop session identity is ambiguous")
	}
	if len(candidates) == 0 {
		return false, nil
	}
	if err := claudeDesktopValidateBoundOptions(candidates[0]); err != nil {
		return false, err
	}
	*target = candidates[0]
	return true, nil
}

func claudeDesktopExactPath(expected, actual string) bool {
	expected = strings.TrimSpace(expandUser(expected))
	actual = strings.TrimSpace(expandUser(actual))
	if expected == "" || actual == "" || !filepath.IsAbs(expected) || !filepath.IsAbs(actual) {
		return false
	}
	expectedReal, expectedErr := filepath.EvalSymlinks(expected)
	actualReal, actualErr := filepath.EvalSymlinks(actual)
	return expectedErr == nil && actualErr == nil && expectedReal == actualReal
}

func claudeDesktopValidateBoundOptions(target claudeDesktopTarget) error {
	opts := target.requested
	if strings.TrimSpace(opts.Cwd) != "" && !claudeDesktopExactPath(opts.Cwd, target.originCwd) {
		return errors.New("Claude Desktop session origin cwd does not match the requested cwd")
	}
	if mode := strings.ToLower(strings.TrimSpace(opts.Mode)); mode != "" &&
		strings.ToLower(strings.TrimSpace(target.mode)) != mode {
		return errors.New("Claude Desktop session permission mode does not match the requested mode")
	}
	if model := strings.TrimSpace(opts.Model); model != "" && strings.TrimSpace(target.model) != model {
		return errors.New("Claude Desktop session model does not match the requested model")
	}
	return nil
}

func claudeDesktopPreflightOptions(target claudeDesktopTarget) error {
	opts := target.requested
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if strings.TrimSpace(opts.Effort) != "" {
		return errors.New("Claude Desktop cannot prove the requested effort before input")
	}
	if mode != "" && mode != "auto" {
		return errors.New("Claude Desktop cannot prove the requested permission mode before input")
	}
	if target.new {
		// The new-session renderer only writes its exact metadata after Send. The
		// current Auto trigger/menu has no stable, proven AX selector, so an
		// explicit mode or model must choose CLI before the folder deep-link.
		if mode != "" && mode != "auto" || strings.TrimSpace(opts.Model) != "" {
			return errors.New("Claude Desktop cannot prove new-session mode or model before input")
		}
		if cwd := strings.TrimSpace(opts.Cwd); cwd != "" && !filepath.IsAbs(cwd) {
			return errors.New("Claude Desktop requires an absolute new-session cwd")
		}
		return nil
	}
	return claudeDesktopValidateBoundOptions(target)
}

func claudeDesktopDeepLink(target claudeDesktopTarget) (string, error) {
	if target.new {
		u := url.URL{Scheme: "claude", Host: "code", Path: "/new"}
		if cwd := strings.TrimSpace(target.requested.Cwd); cwd != "" {
			if !filepath.IsAbs(cwd) {
				return "", errors.New("Claude Desktop folder deep-link requires an absolute cwd")
			}
			q := u.Query()
			q.Set("folder", filepath.Clean(cwd))
			q.Set("src", "external")
			u.RawQuery = q.Encode()
		}
		return u.String(), nil
	}
	if !claudeUUIDRE.MatchString(target.cliID) {
		return "", errors.New("Claude Desktop resume requires a transcript UUID")
	}
	u := url.URL{Scheme: "claude", Host: "resume"}
	q := u.Query()
	q.Set("session", target.cliID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type claudeAXElement struct {
	Role       string `json:"role"`
	Identifier string `json:"identifier"`
	Subrole    string `json:"subrole"`
	Label      string `json:"label"`
	Value      string `json:"value"`
	Enabled    *bool  `json:"enabled"`
	Selected   *bool  `json:"selected"`
	Focused    *bool  `json:"focused"`
	Current    *bool  `json:"current"`
	Checked    *bool  `json:"checked"`
	Pressed    *bool  `json:"pressed"`
	Actionable bool   `json:"actionable"`
	Path       []int  `json:"path"`
}

func claudeAXBinaryState(element claudeAXElement) (bool, bool) {
	known := false
	value := false
	for _, candidate := range []*bool{element.Checked, element.Pressed, element.Selected} {
		if candidate == nil {
			continue
		}
		if known && value != *candidate {
			return false, false
		}
		known = true
		value = *candidate
	}
	return value, known
}

type claudeAXSnapshot struct {
	elements []claudeAXElement
}

func (snapshot claudeAXSnapshot) exactPath(path []int) (claudeAXElement, error) {
	matches := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		if len(element.Path) != len(path) {
			continue
		}
		equal := true
		for index := range path {
			if element.Path[index] != path[index] {
				equal = false
				break
			}
		}
		if equal {
			matches = append(matches, element)
		}
	}
	if len(matches) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop exact AX path is missing or ambiguous")
	}
	return matches[0], nil
}

func parseClaudeAXSnapshot(result ComputerUseToolResult) (claudeAXSnapshot, error) {
	var envelope struct {
		Accessibility struct {
			Elements []claudeAXElement `json:"elements"`
		} `json:"accessibility"`
		Elements []claudeAXElement `json:"elements"`
	}
	if err := json.Unmarshal([]byte(result.Text), &envelope); err != nil {
		return claudeAXSnapshot{}, errors.New("Claude Desktop returned invalid Accessibility state")
	}
	elements := envelope.Accessibility.Elements
	if elements == nil {
		elements = envelope.Elements
	}
	if len(elements) > 2000 {
		return claudeAXSnapshot{}, errors.New("Claude Desktop Accessibility state is too large")
	}
	seen := map[string]bool{}
	for _, element := range elements {
		// AXApplication itself is path=[] and can legitimately carry a label.
		// Only a selected mutation target must have a non-root path.
		if len(element.Path) > 40 {
			return claudeAXSnapshot{}, errors.New("Claude Desktop returned an invalid Accessibility path")
		}
		parts := make([]string, len(element.Path))
		for index, component := range element.Path {
			if component < 0 {
				return claudeAXSnapshot{}, errors.New("Claude Desktop returned an invalid Accessibility path")
			}
			parts[index] = fmt.Sprint(component)
		}
		key := strings.Join(parts, "/")
		if seen[key] {
			return claudeAXSnapshot{}, errors.New("Claude Desktop returned duplicate Accessibility paths")
		}
		seen[key] = true
	}
	return claudeAXSnapshot{elements: elements}, nil
}

func claudeAXText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func (snapshot claudeAXSnapshot) unique(match func(claudeAXElement) bool) (claudeAXElement, error) {
	var found claudeAXElement
	count := 0
	for _, element := range snapshot.elements {
		if match(element) {
			found = element
			count++
		}
	}
	if count == 0 {
		return claudeAXElement{}, errors.New("required Claude Desktop element was not found")
	}
	if count != 1 {
		return claudeAXElement{}, errors.New("required Claude Desktop element is ambiguous")
	}
	return found, nil
}

func (snapshot claudeAXSnapshot) hasExactText(text string) bool {
	want := claudeAXText(text)
	if want == "" {
		return false
	}
	for _, element := range snapshot.elements {
		if claudeAXText(element.Label) == want || claudeAXText(element.Value) == want {
			return true
		}
	}
	return false
}

func claudeComposer(snapshot claudeAXSnapshot) (claudeAXElement, error) {
	accepted := map[string]bool{
		"message claude": true, "reply to claude": true, "message": true,
		"how can claude help you today?": true, "describe a task": true,
	}
	return snapshot.unique(func(element claudeAXElement) bool {
		if element.Role != "AXTextArea" && element.Role != "AXTextField" {
			return false
		}
		return accepted[claudeAXText(element.Label)]
	})
}

func claudeSendButton(snapshot claudeAXSnapshot) (claudeAXElement, error) {
	accepted := map[string]bool{"send": true, "send message": true, "submit message": true}
	return snapshot.unique(func(element claudeAXElement) bool {
		return element.Role == "AXButton" && element.Actionable && accepted[claudeAXText(element.Label)]
	})
}

func claudeModeTrigger(snapshot claudeAXSnapshot) (claudeAXElement, error) {
	composer, err := claudeComposer(snapshot)
	if err != nil {
		return claudeAXElement{}, err
	}
	send, err := claudeSendButton(snapshot)
	if err != nil {
		return claudeAXElement{}, err
	}
	labels := map[string]bool{
		"manual": true, "accept edits": true, "plan": true, "auto": true, "bypass permissions": true,
	}
	bestScore := -1
	best := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		enabled := element.Enabled == nil || *element.Enabled
		if element.Role != "AXButton" || !element.Actionable || !enabled || !labels[claudeAXText(element.Label)] {
			continue
		}
		composerScore := claudeCommonPathPrefix(element.Path, composer.Path)
		sendScore := claudeCommonPathPrefix(element.Path, send.Path)
		score := composerScore
		if sendScore < score {
			score = sendScore
		}
		if score > bestScore {
			bestScore = score
			best = []claudeAXElement{element}
		} else if score == bestScore {
			best = append(best, element)
		}
	}
	if bestScore <= 0 || len(best) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop mode trigger is missing or ambiguous")
	}
	return best[0], nil
}

func claudeWorkingDirectoryTrigger(snapshot claudeAXSnapshot, cwd string) (claudeAXElement, error) {
	realCwd, err := filepath.EvalSymlinks(strings.TrimSpace(expandUser(cwd)))
	if err != nil || !filepath.IsAbs(realCwd) {
		return claudeAXElement{}, errors.New("Claude Desktop working directory is not canonical")
	}
	composer, err := claudeComposer(snapshot)
	if err != nil {
		return claudeAXElement{}, err
	}
	send, err := claudeSendButton(snapshot)
	if err != nil {
		return claudeAXElement{}, err
	}
	want := claudeAXText(filepath.Base(realCwd))
	bestScore := -1
	best := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		enabled := element.Enabled == nil || *element.Enabled
		if element.Role != "AXButton" || !element.Actionable || !enabled || claudeAXText(element.Label) != want {
			continue
		}
		score := claudeCommonPathPrefix(element.Path, composer.Path)
		if sendScore := claudeCommonPathPrefix(element.Path, send.Path); sendScore < score {
			score = sendScore
		}
		if score > bestScore {
			bestScore = score
			best = []claudeAXElement{element}
		} else if score == bestScore {
			best = append(best, element)
		}
	}
	if bestScore <= 0 || len(best) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop working-directory trigger is missing or ambiguous")
	}
	return best[0], nil
}

func claudeWorkingDirectoryTooltip(
	snapshot claudeAXSnapshot, trigger claudeAXElement, cwd string,
) error {
	realCwd, err := filepath.EvalSymlinks(strings.TrimSpace(expandUser(cwd)))
	if err != nil {
		return errors.New("Claude Desktop working directory is not canonical")
	}
	headers := make([]claudeAXElement, 0, 1)
	paths := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		label, value := strings.TrimSpace(element.Label), strings.TrimSpace(element.Value)
		if claudeAXText(label) == "working directory" || claudeAXText(value) == "working directory" {
			headers = append(headers, element)
		}
		if label == realCwd || value == realCwd {
			paths = append(paths, element)
		}
	}
	if len(headers) != 1 || len(paths) != 1 {
		return errors.New("Claude Desktop working-directory tooltip identity is missing or ambiguous")
	}
	header, path := headers[0], paths[0]
	currentTrigger, err := snapshot.exactPath(trigger.Path)
	if err != nil || currentTrigger.Focused == nil || !*currentTrigger.Focused ||
		claudeAXText(currentTrigger.Label) != claudeAXText(filepath.Base(realCwd)) {
		return errors.New("Claude Desktop working-directory trigger is not the exact focused owner")
	}
	if claudeCommonPathPrefix(header.Path, path.Path) <= 0 ||
		claudeCommonPathPrefix(trigger.Path, header.Path) < 2 ||
		claudeCommonPathPrefix(trigger.Path, path.Path) < 2 {
		return errors.New("Claude Desktop working-directory tooltip is outside the exact composer surface")
	}
	return nil
}

func claudeModeAutoOption(snapshot claudeAXSnapshot) (claudeAXElement, bool, error) {
	header, err := snapshot.unique(func(element claudeAXElement) bool {
		roleOK := element.Role == "AXHeading" || element.Role == "AXStaticText" || element.Role == "AXHeader"
		return roleOK && (claudeAXText(element.Label) == "mode" || claudeAXText(element.Value) == "mode")
	})
	if err != nil {
		return claudeAXElement{}, false, errors.New("Claude Desktop mode popup header is missing or ambiguous")
	}
	bestScore := -1
	best := make([]claudeAXElement, 0, 1)
	states := make([]bool, 0, 1)
	for _, element := range snapshot.elements {
		roleOK := element.Role == "AXMenuItem" || element.Role == "AXRadioButton" ||
			element.Role == "AXButton" || element.Role == "AXMenuItemRadio"
		enabled := element.Enabled == nil || *element.Enabled
		selected, known := claudeAXBinaryState(element)
		if !roleOK || !element.Actionable || !enabled || !known || claudeAXText(element.Label) != "auto" {
			continue
		}
		score := claudeCommonPathPrefix(header.Path, element.Path)
		if score > bestScore {
			bestScore = score
			best = []claudeAXElement{element}
			states = []bool{selected}
		} else if score == bestScore {
			best = append(best, element)
			states = append(states, selected)
		}
	}
	if bestScore <= 0 || len(best) != 1 {
		return claudeAXElement{}, false, errors.New("Claude Desktop Auto mode item is missing or ambiguous")
	}
	return best[0], states[0], nil
}

func claudeAutoModeConfirmation(
	snapshot claudeAXSnapshot, cwd string,
) (claudeAXElement, bool, error) {
	titles := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		if claudeAXText(element.Label) == "enable auto mode?" || claudeAXText(element.Value) == "enable auto mode?" {
			titles = append(titles, element)
		}
	}
	if len(titles) == 0 {
		return claudeAXElement{}, false, nil
	}
	if len(titles) != 1 {
		return claudeAXElement{}, true, errors.New("Claude Desktop Auto confirmation is ambiguous")
	}
	title := titles[0]
	buttons := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		enabled := element.Enabled == nil || *element.Enabled
		if element.Role != "AXButton" || !element.Actionable || !enabled ||
			claudeAXText(element.Label) != "enable auto mode" {
			continue
		}
		buttons = append(buttons, element)
	}
	if len(buttons) != 1 || claudeCommonPathPrefix(title.Path, buttons[0].Path) <= 0 {
		return claudeAXElement{}, true, errors.New("Claude Desktop Auto confirmation button is missing or ambiguous")
	}
	if strings.TrimSpace(cwd) != "" {
		realCwd, err := filepath.EvalSymlinks(strings.TrimSpace(expandUser(cwd)))
		if err != nil {
			return claudeAXElement{}, true, errors.New("Claude Desktop Auto confirmation cwd is not canonical")
		}
		matches := 0
		for _, element := range snapshot.elements {
			if (strings.TrimSpace(element.Label) == realCwd || strings.TrimSpace(element.Value) == realCwd) &&
				claudeCommonPathPrefix(title.Path, element.Path) > 0 {
				matches++
			}
		}
		if matches != 1 {
			return claudeAXElement{}, true, errors.New("Claude Desktop Auto confirmation workspace does not match")
		}
	}
	return buttons[0], true, nil
}

func claudeNewSessionMarker(snapshot claudeAXSnapshot) bool {
	markers := map[string]bool{"new task": true, "new session": true, "start new session": true}
	for _, element := range snapshot.elements {
		if element.Current != nil && *element.Current &&
			(markers[claudeAXText(element.Label)] || markers[claudeAXText(element.Value)]) {
			return true
		}
	}
	return false
}

func claudePromptRecordIDs(records []map[string]any) map[string]bool {
	ids := map[string]bool{}
	for _, record := range records {
		if id := stringAny(record["uuid"]); id != "" {
			ids[id] = true
		}
	}
	return ids
}

func claudeRawUserPrompt(record map[string]any) (string, bool) {
	if stringAny(record["type"]) != "user" {
		return "", false
	}
	content := mapAny(record["message"])["content"]
	if text, ok := content.(string); ok {
		return text, true
	}
	blocks := listAny(content)
	if len(blocks) != 1 {
		return "", false
	}
	block := mapAny(blocks[0])
	if stringAny(block["type"]) != "text" {
		return "", false
	}
	return stringAny(block["text"]), true
}

func claudePromptTranscriptCount(
	records []map[string]any, prompt string, startedAt time.Time, baseline map[string]bool,
) int {
	count := 0
	for _, record := range records {
		text, ok := claudeRawUserPrompt(record)
		if !ok || text != prompt {
			continue
		}
		id := stringAny(record["uuid"])
		if id == "" || baseline[id] {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, stringAny(record["timestamp"]))
		if err != nil || ts.Before(startedAt) {
			continue
		}
		count++
	}
	return count
}

type claudeDesktopTransaction struct {
	tool            ComputerUseToolHandler
	bundleID        string
	target          *claudeDesktopTarget
	validate        func() error
	mutated         bool
	confirmed       bool
	noFallback      bool
	modeOpen        bool
	autoConfirmOpen bool
}

func (tx *claudeDesktopTransaction) inspect(ctx context.Context) (claudeAXSnapshot, error) {
	result, err := tx.tool(ctx, ComputerUseToolRequest{
		Tool: "get_app_state", BundleID: tx.bundleID,
	})
	if err != nil {
		return claudeAXSnapshot{}, err
	}
	return parseClaudeAXSnapshot(result)
}

func claudeComputerUseSecurityRefusal(err error) bool {
	return errors.Is(err, ErrComputerUseAutomationCleanup) ||
		errors.Is(err, computeruse.ErrLocalInput) ||
		errors.Is(err, computeruse.ErrWindowBusy) ||
		errors.Is(err, computeruse.ErrTurnNotActive) ||
		errors.Is(err, computeruse.ErrNoWindow) ||
		errors.Is(err, computeruse.ErrBadRequest) ||
		errors.Is(err, computeruse.ErrShieldRequired) ||
		errors.Is(err, computeruse.ErrNotArmed)
}

func claudeComputerUseCapabilityAbsence(err error) bool {
	return errors.Is(err, computeruse.ErrUnsupported) ||
		errors.Is(err, computeruse.ErrHelperUnavailable) ||
		errors.Is(err, computeruse.ErrNotEnabled) ||
		errors.Is(err, computeruse.ErrLockedUseNotEnabled)
}

func (tx *claudeDesktopTransaction) verifyTarget(snapshot claudeAXSnapshot) error {
	if tx.target.new {
		if tx.autoConfirmOpen {
			_, found, modalErr := claudeAutoModeConfirmation(snapshot, tx.target.requested.Cwd)
			if found && modalErr == nil {
				return nil
			}
			return errors.New("Claude Desktop Auto confirmation surface could not be verified")
		}
		composer, composerErr := claudeComposer(snapshot)
		focused := composerErr == nil && composer.Focused != nil && *composer.Focused
		modePopup := false
		if tx.modeOpen {
			_, _, modeErr := claudeModeAutoOption(snapshot)
			modePopup = modeErr == nil
		}
		if !claudeNewSessionMarker(snapshot) || composerErr != nil || (!focused && !modePopup) {
			return errors.New("Claude Desktop new-session surface could not be verified")
		}
		return nil
	}
	matches := 0
	for _, element := range snapshot.elements {
		current := element.Current != nil && *element.Current
		if !current {
			continue
		}
		textMatch := claudeAXText(element.Label) == claudeAXText(tx.target.title) ||
			claudeAXText(element.Value) == claudeAXText(tx.target.title)
		identifier := claudeAXText(element.Identifier)
		identityMatch := tx.target.cliID != "" && strings.Contains(identifier, claudeAXText(tx.target.cliID))
		identityMatch = identityMatch || tx.target.desktopID != "" && strings.Contains(identifier, claudeAXText(tx.target.desktopID))
		if identityMatch || (tx.target.titleUnique && textMatch) {
			matches++
		}
	}
	if matches != 1 {
		return errors.New("Claude Desktop did not expose the exact target session")
	}
	return nil
}

func (tx *claudeDesktopTransaction) focusNewComposer(ctx context.Context) error {
	snapshot, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	if !tx.target.new || !claudeNewSessionMarker(snapshot) {
		return errors.New("Claude Desktop new-session surface is no longer exact")
	}
	composer, err := claudeComposer(snapshot)
	if err != nil {
		return err
	}
	if composer.Focused != nil && *composer.Focused {
		return nil
	}
	if len(composer.Path) == 0 {
		return errors.New("Claude Desktop refused to focus an application root element")
	}
	tx.mutated = true
	if _, err := tx.tool(ctx, ComputerUseToolRequest{
		Tool: "focus", BundleID: tx.bundleID, Path: append([]int(nil), composer.Path...),
	}); err != nil {
		return err
	}
	fresh, err := tx.inspect(ctx)
	if err != nil || !claudeNewSessionMarker(fresh) {
		return errors.New("Claude Desktop composer focus could not be verified")
	}
	composer, err = claudeComposer(fresh)
	if err != nil || composer.Focused == nil || !*composer.Focused {
		return errors.New("Claude Desktop composer did not become focused")
	}
	return nil
}

func (tx *claudeDesktopTransaction) configureNewAutoMode(ctx context.Context) error {
	if !tx.target.new || strings.ToLower(strings.TrimSpace(tx.target.requested.Mode)) != "auto" {
		return nil
	}
	snapshot, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	if err := tx.verifyTarget(snapshot); err != nil {
		return err
	}
	trigger, err := claudeModeTrigger(snapshot)
	if err != nil {
		return err
	}
	if claudeAXText(trigger.Label) == "auto" {
		return nil
	}
	if err := tx.press(ctx, claudeModeTrigger); err != nil {
		return err
	}
	tx.modeOpen = true
	popup, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	auto, selected, err := claudeModeAutoOption(popup)
	if err != nil || selected {
		return errors.New("Claude Desktop Auto mode item state is inconsistent")
	}
	if err := tx.press(ctx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
		candidate, state, err := claudeModeAutoOption(snapshot)
		if err != nil || state {
			return claudeAXElement{}, errors.New("Claude Desktop Auto mode item is not an exact unchecked choice")
		}
		return candidate, nil
	}); err != nil {
		return err
	}
	_ = auto
	post, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	confirm, hasConfirm, confirmErr := claudeAutoModeConfirmation(post, tx.target.requested.Cwd)
	if confirmErr != nil {
		return confirmErr
	}
	if hasConfirm {
		tx.autoConfirmOpen = true
		if err := tx.press(ctx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
			candidate, found, err := claudeAutoModeConfirmation(snapshot, tx.target.requested.Cwd)
			if err != nil || !found {
				return claudeAXElement{}, errors.New("Claude Desktop Auto confirmation is no longer exact")
			}
			return candidate, nil
		}); err != nil {
			return err
		}
		_ = confirm
		tx.autoConfirmOpen = false
		post, err = tx.inspect(ctx)
		if err != nil {
			return err
		}
	}
	trigger, triggerErr := claudeModeTrigger(post)
	if triggerErr == nil && claudeAXText(trigger.Label) == "auto" {
		tx.modeOpen = false
		return tx.focusNewComposer(ctx)
	}
	_, selected, optionErr := claudeModeAutoOption(post)
	if optionErr != nil || !selected {
		return errors.New("Claude Desktop Auto mode selection could not be verified")
	}
	// The popup remained open after selection. Close it through the exact footer
	// trigger, then restore and verify composer focus before any text mutation.
	if err := tx.press(ctx, claudeModeTrigger); err != nil {
		return err
	}
	tx.modeOpen = false
	return tx.focusNewComposer(ctx)
}

func (tx *claudeDesktopTransaction) verifyNewWorkingDirectory(
	ctx context.Context, deps claudeComputerUseDependencies,
) error {
	if !tx.target.new || strings.TrimSpace(tx.target.requested.Cwd) == "" {
		return nil
	}
	snapshot, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	if err := tx.verifyTarget(snapshot); err != nil {
		return err
	}
	trigger, err := claudeWorkingDirectoryTrigger(snapshot, tx.target.requested.Cwd)
	if err != nil {
		return err
	}
	if len(trigger.Path) == 0 {
		return errors.New("Claude Desktop refused to focus an application root element")
	}
	tx.mutated = true
	if _, err := tx.tool(ctx, ComputerUseToolRequest{
		Tool: "focus", BundleID: tx.bundleID, Path: append([]int(nil), trigger.Path...),
	}); err != nil {
		return err
	}
	if err := deps.sleep(ctx, 500*time.Millisecond); err != nil {
		return err
	}
	proof, err := tx.inspect(ctx)
	if err != nil || !claudeNewSessionMarker(proof) {
		return errors.New("Claude Desktop working-directory tooltip could not be inspected")
	}
	if err := claudeWorkingDirectoryTooltip(proof, trigger, tx.target.requested.Cwd); err != nil {
		return err
	}
	return tx.focusNewComposer(ctx)
}

func (tx *claudeDesktopTransaction) setValue(
	ctx context.Context, selectElement func(claudeAXSnapshot) (claudeAXElement, error), value string,
) error {
	snapshot, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	if err := tx.verifyTarget(snapshot); err != nil {
		return err
	}
	element, err := selectElement(snapshot)
	if err != nil {
		return err
	}
	if len(element.Path) == 0 {
		return errors.New("Claude Desktop refused to mutate an application root element")
	}
	if tx.validate != nil {
		if err := tx.validate(); err != nil {
			return err
		}
	}
	tx.mutated = true
	_, err = tx.tool(ctx, ComputerUseToolRequest{
		Tool: "set_value", BundleID: tx.bundleID, Path: append([]int(nil), element.Path...), Value: &value,
	})
	return err
}

func (tx *claudeDesktopTransaction) press(
	ctx context.Context, selectElement func(claudeAXSnapshot) (claudeAXElement, error),
) error {
	snapshot, err := tx.inspect(ctx)
	if err != nil {
		return err
	}
	if err := tx.verifyTarget(snapshot); err != nil {
		return err
	}
	element, err := selectElement(snapshot)
	if err != nil {
		return err
	}
	if len(element.Path) == 0 {
		return errors.New("Claude Desktop refused to mutate an application root element")
	}
	if tx.validate != nil {
		if err := tx.validate(); err != nil {
			return err
		}
	}
	tx.mutated = true
	_, err = tx.tool(ctx, ComputerUseToolRequest{
		Tool: "press", BundleID: tx.bundleID, Path: append([]int(nil), element.Path...),
	})
	return err
}

func (c *Claude) claudeComputerUseSendPrompt(
	ctx context.Context, sessionID, prompt string, promptSpecs ...*claudePromptAttemptSpec,
) claudeComputerUseOutcome {
	outcome := claudeComputerUseOutcome{Disposition: claudeComputerUseNotAttempted}
	if sessionID == "" || strings.TrimSpace(prompt) == "" {
		outcome.Err = errors.New("Claude Desktop prompt requires a logical session and non-empty text")
		return outcome
	}
	handler, deps, done, err := c.beginClaudeComputerUse(sessionID)
	if err != nil {
		outcome.Err = err
		outcome.FallbackAllowed = errors.Is(err, errClaudeComputerUseUnavailable)
		return outcome
	}
	defer done()
	target, err := c.claudeDesktopTarget(sessionID, deps.sessions())
	if err != nil {
		outcome.Err = err
		return outcome
	}
	if err := claudeDesktopPreflightOptions(target); err != nil {
		outcome.Err = err
		outcome.FallbackAllowed = true
		return outcome
	}
	if target.cliID != "" && !turnstateIdle(target.cliID, c.turnstateDir, c.staleAfter) {
		outcome.Err = errClaudeDesktopTurnRunning
		return outcome
	}
	var promptSpec *claudePromptAttemptSpec
	if len(promptSpecs) > 0 {
		promptSpec = promptSpecs[0]
	}
	if err := c.rejectKnownClaudePromptAttempt(sessionID, claudeRouteDesktopComputerUse, promptSpec); err != nil {
		outcome.Err = errors.New("Claude Desktop prompt operation was already attempted; delivery is unknown")
		outcome.Disposition = claudeComputerUseDeliveryUnknown
		return outcome
	}
	deepLink, err := claudeDesktopDeepLink(target)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	appPath := stringExtra(c.cfg.Extra, "desktop_app_path", claudeDesktopDefaultAppPath)
	bundleID := stringExtra(c.cfg.Extra, "desktop_bundle_id", claudeDesktopDefaultBundleID)
	teamID := stringExtra(c.cfg.Extra, "desktop_team_id", claudeDesktopDefaultTeamID)
	if err := deps.verifyApp(ctx, appPath, bundleID, teamID); err != nil {
		outcome.Err = err
		outcome.FallbackAllowed = true
		return outcome
	}
	// Background launch is capability preflight only: it carries no session,
	// prompt, or decision and deliberately does not steal foreground focus.
	if err := deps.launchApp(ctx, appPath); err != nil {
		outcome.Err = errors.New("Claude Desktop could not start in the background")
		outcome.FallbackAllowed = true
		return outcome
	}
	if err := deps.waitApp(ctx, appPath); err != nil {
		outcome.Err = err
		outcome.FallbackAllowed = true
		return outcome
	}
	startedAt := deps.now().UTC()
	baselineRecords := map[string]bool{}
	if target.cliID != "" {
		baselineRecords = claudePromptRecordIDs(deps.records(target.cliID))
	}
	var tx *claudeDesktopTransaction
	var promptAttempt turnstatehook.InteractionAttempt
	err = handler(ctx, sessionID, func(operationCtx context.Context, tool ComputerUseToolHandler) error {
		tx = &claudeDesktopTransaction{tool: tool, bundleID: bundleID, target: &target}
		// Establish the helper's shield/input guard before any navigation can
		// activate Claude or alter visible UI. A failure here remains eligible for
		// a brand-new session's CLI fallback because no application mutation ran.
		initial, inspectErr := tx.inspect(operationCtx)
		if claudeComputerUseSecurityRefusal(inspectErr) {
			tx.noFallback = true
			return inspectErr
		}
		if inspectErr != nil {
			return inspectErr
		}
		if len(initial.elements) == 0 {
			return errors.New("Claude Desktop exposed no Accessibility surface")
		}
		// Deep-link delivery can partially navigate even when open reports an
		// error. Mark it before the call: from this point onward CLI retry could
		// select a second owner and is forbidden.
		if err := c.commitClaudeControlRoute(
			operationCtx, sessionID, claudeRouteDesktopComputerUse,
		); err != nil {
			tx.noFallback = true
			return errors.New("Claude Desktop route could not be committed")
		}
		promptAttempt, err = c.beginClaudePromptAttempt(sessionID, claudeRouteDesktopComputerUse, promptSpec)
		if err != nil {
			tx.noFallback = true
			return errors.New("Claude Desktop prompt operation was already attempted or could not be recorded")
		}
		tx.mutated = true
		if err := deps.openURL(operationCtx, appPath, deepLink); err != nil {
			return errors.New("Claude Desktop could not open the target surface")
		}
		for {
			ready, readyErr := tx.inspect(operationCtx)
			if readyErr != nil {
				return readyErr
			}
			readyErr = tx.verifyTarget(ready)
			if readyErr == nil {
				_, readyErr = claudeComposer(ready)
			}
			if readyErr == nil {
				break
			}
			if err := deps.sleep(operationCtx, 100*time.Millisecond); err != nil {
				return err
			}
		}
		if err := tx.verifyNewWorkingDirectory(operationCtx, deps); err != nil {
			return err
		}
		if err := tx.configureNewAutoMode(operationCtx); err != nil {
			return err
		}
		if err := tx.setValue(operationCtx, claudeComposer, prompt); err != nil {
			return err
		}
		var submittedComposerPath []int
		if err := tx.press(operationCtx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
			composer, err := claudeComposer(snapshot)
			if err != nil {
				return claudeAXElement{}, err
			}
			if composer.Value != prompt {
				return claudeAXElement{}, errors.New("Claude Desktop did not retain the pending prompt")
			}
			submittedComposerPath = append([]int(nil), composer.Path...)
			return claudeSendButton(snapshot)
		}); err != nil {
			return err
		}
		snapshot, inspectErr := tx.inspect(operationCtx)
		if inspectErr != nil {
			return inspectErr
		}
		if target.new {
			// New-session metadata is written asynchronously after Send. Confirm only
			// that the exact composer path we just operated was cleared, then return
			// immediately so the automation handler can close/relock. Native identity
			// and transcript polling happen outside the callback below.
			composer, composerErr := snapshot.exactPath(submittedComposerPath)
			if composerErr != nil || (composer.Role != "AXTextArea" && composer.Role != "AXTextField") ||
				composer.Value == prompt {
				return errors.New("Claude Desktop did not acknowledge new-session prompt submission")
			}
		} else {
			if err := tx.verifyTarget(snapshot); err != nil {
				return err
			}
			composer, composerErr := claudeComposer(snapshot)
			if composerErr != nil || composer.Value == prompt {
				return errors.New("Claude Desktop did not acknowledge prompt submission")
			}
		}
		tx.confirmed = true
		return nil
	})
	if tx != nil && tx.mutated {
		c.bindClaudeComputerUseRoute(sessionID, claudeRouteDesktopComputerUse)
	}
	if err != nil {
		outcome.Err = err
		capabilityAbsent := claudeComputerUseCapabilityAbsence(err) && !claudeComputerUseSecurityRefusal(err) &&
			tx != nil && !tx.mutated && !tx.noFallback
		if capabilityAbsent {
			outcome.FallbackAllowed = true
		} else {
			outcome.Disposition = claudeComputerUseDeliveryUnknown
		}
		return outcome
	}
	if tx == nil || !tx.mutated || !tx.confirmed {
		outcome.Err = errors.New("Claude Desktop operation returned without a delivery confirmation")
		if tx != nil && tx.mutated {
			outcome.Disposition = claudeComputerUseDeliveryUnknown
		}
		return outcome
	}
	for {
		bound, bindErr := claudeNewDesktopSession(&target, deps.sessions())
		if bindErr != nil {
			outcome.Err = bindErr
			outcome.Disposition = claudeComputerUseDeliveryUnknown
			return outcome
		}
		if bound && target.cliID != "" {
			count := claudePromptTranscriptCount(
				deps.records(target.cliID), prompt, startedAt, baselineRecords,
			)
			if count > 1 {
				outcome.Err = errors.New("Claude Desktop appended the prompt more than once")
				outcome.Disposition = claudeComputerUseDeliveryUnknown
				return outcome
			}
			if count == 1 {
				break
			}
		}
		if err := deps.sleep(ctx, 100*time.Millisecond); err != nil {
			outcome.Err = errors.New("Claude Desktop prompt transcript confirmation timed out")
			outcome.Disposition = claudeComputerUseDeliveryUnknown
			return outcome
		}
	}
	if err := c.resolveClaudePromptAttempt(promptAttempt); err != nil {
		outcome.Err = errors.New("Claude Desktop prompt delivery could not be durably resolved")
		outcome.Disposition = claudeComputerUseDeliveryUnknown
		return outcome
	}
	outcome.Disposition = claudeComputerUseConfirmed
	outcome.DesktopSessionID = target.desktopID
	outcome.TranscriptID = target.cliID
	return outcome
}

func claudeAXContains(element claudeAXElement, needle string) bool {
	want := claudeAXText(needle)
	if want == "" {
		return false
	}
	return strings.Contains(claudeAXText(element.Label), want) ||
		strings.Contains(claudeAXText(element.Value), want)
}

func claudeCommonPathPrefix(left, right []int) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return limit
}

func claudeUniqueNearAnchor(
	snapshot claudeAXSnapshot,
	anchor func(claudeAXElement) bool,
	candidate func(claudeAXElement) bool,
) (claudeAXElement, error) {
	anchors := make([]claudeAXElement, 0, 2)
	candidates := make([]claudeAXElement, 0, 2)
	for _, element := range snapshot.elements {
		if anchor(element) {
			anchors = append(anchors, element)
		}
		if candidate(element) {
			candidates = append(candidates, element)
		}
	}
	if len(anchors) == 0 || len(candidates) == 0 {
		return claudeAXElement{}, errors.New("Claude Desktop request card is not present")
	}
	bestScore := -1
	best := make([]claudeAXElement, 0, 1)
	for _, candidate := range candidates {
		score := 0
		for _, anchor := range anchors {
			if prefix := claudeCommonPathPrefix(anchor.Path, candidate.Path); prefix > score {
				score = prefix
			}
		}
		if score > bestScore {
			bestScore = score
			best = []claudeAXElement{candidate}
		} else if score == bestScore {
			best = append(best, candidate)
		}
	}
	if bestScore <= 0 || len(best) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop request card is ambiguous")
	}
	return best[0], nil
}

func claudePathDescendsFrom(path, root []int) bool {
	if len(root) == 0 || len(path) <= len(root) {
		return false
	}
	for index := range root {
		if path[index] != root[index] {
			return false
		}
	}
	return true
}

func claudePermissionCard(
	snapshot claudeAXSnapshot, record turnstatehook.InteractionRecord,
) (claudeAXElement, error) {
	detail := claudeAXText(toolDetail(record.ToolInput, 240))
	return snapshot.unique(func(root claudeAXElement) bool {
		if root.Role != "AXGroup" || !strings.HasPrefix(claudeAXText(root.Label), "permission request:") {
			return false
		}
		for _, child := range snapshot.elements {
			if !claudePathDescendsFrom(child.Path, root.Path) {
				continue
			}
			text := claudeAXText(firstNonEmpty(child.Label, child.Value))
			if detail != "" && strings.Contains(text, detail) {
				return true
			}
			if detail == "" && claudeAXContains(child, record.ToolName) {
				return true
			}
		}
		return false
	})
}

func claudeApprovalButton(
	snapshot claudeAXSnapshot, record turnstatehook.InteractionRecord, decision string,
) (claudeAXElement, error) {
	labels := map[string]bool{}
	if decision == "allow" {
		// App bundle inspection confirms that broader buttons coexist. Only the
		// exact one-shot label maps to the API's temporary allow decision.
		labels["allow once"] = true
	} else if decision == "deny" {
		labels["deny"] = true
	} else {
		return claudeAXElement{}, errors.New("Claude Desktop decision must be allow or deny")
	}
	card, err := claudePermissionCard(snapshot, record)
	if err != nil {
		return claudeAXElement{}, err
	}
	return snapshot.unique(func(element claudeAXElement) bool {
		enabled := element.Enabled != nil && *element.Enabled
		return claudePathDescendsFrom(element.Path, card.Path) && element.Role == "AXButton" &&
			element.Actionable && enabled && labels[claudeAXText(element.Label)]
	})
}

func claudeApprovalCardActionable(
	snapshot claudeAXSnapshot, record turnstatehook.InteractionRecord,
) bool {
	card, err := claudePermissionCard(snapshot, record)
	if err != nil {
		return false
	}
	for _, element := range snapshot.elements {
		label := claudeAXText(element.Label)
		if claudePathDescendsFrom(element.Path, card.Path) && element.Role == "AXButton" &&
			element.Actionable && (label == "allow once" || label == "deny") {
			return true
		}
	}
	return false
}

type claudeQuestionPlan struct {
	question string
	multi    bool
	selected []string
	other    string
}

type claudeInteractionAttemptSpec struct {
	record   turnstatehook.InteractionRecord
	kind     string
	decision any
}

func (c *Claude) beginClaudeInteractionAttempt(
	sessionID string, spec *claudeInteractionAttemptSpec,
) (turnstatehook.InteractionAttempt, error) {
	if spec == nil {
		return turnstatehook.InteractionAttempt{}, nil
	}
	identityDigest, err := turnstatehook.InteractionIdentityDigest(spec.record)
	if err != nil {
		return turnstatehook.InteractionAttempt{}, err
	}
	decisionDigest, err := turnstatehook.InteractionDecisionDigest(spec.decision)
	if err != nil {
		return turnstatehook.InteractionAttempt{}, err
	}
	return turnstatehook.BeginInteractionAttempt(
		stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"),
		spec.record.RequestID, identityDigest, sessionID, spec.kind, decisionDigest,
	)
}

func (c *Claude) resolveClaudeInteractionAttempt(attempt turnstatehook.InteractionAttempt) error {
	if attempt.RequestID == "" || attempt.OperationID == "" {
		return nil
	}
	_, err := turnstatehook.ResolveInteractionAttempt(
		stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"),
		attempt.RequestID, attempt.OperationID,
	)
	return err
}

func (c *Claude) beginClaudePromptAttempt(
	sessionID, route string, spec *claudePromptAttemptSpec,
) (turnstatehook.InteractionAttempt, error) {
	if spec == nil || strings.TrimSpace(spec.operationID) == "" {
		return turnstatehook.InteractionAttempt{}, nil
	}
	requestID, identityDigest, decisionDigest, err := claudePromptAttemptMaterial(sessionID, route, spec)
	if err != nil {
		return turnstatehook.InteractionAttempt{}, err
	}
	return turnstatehook.BeginInteractionAttempt(
		stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"),
		requestID, identityDigest, sessionID, "prompt", decisionDigest,
	)
}

func claudePromptAttemptMaterial(
	sessionID, route string, spec *claudePromptAttemptSpec,
) (string, string, string, error) {
	requestID := "prompt:" + strings.TrimSpace(spec.operationID)
	identityDigest, err := turnstatehook.InteractionDecisionDigest(struct {
		OperationID string `json:"operation_id"`
		SessionID   string `json:"session_id"`
		Route       string `json:"route"`
	}{OperationID: spec.operationID, SessionID: sessionID, Route: route})
	if err != nil {
		return "", "", "", err
	}
	decisionDigest, err := turnstatehook.InteractionDecisionDigest(struct {
		Prompt      string       `json:"prompt"`
		Attachments []Attachment `json:"attachments,omitempty"`
	}{Prompt: spec.prompt, Attachments: spec.attachments})
	if err != nil {
		return "", "", "", err
	}
	return requestID, identityDigest, decisionDigest, nil
}

func (c *Claude) rejectKnownClaudePromptAttempt(
	sessionID, route string, spec *claudePromptAttemptSpec,
) error {
	if spec == nil || strings.TrimSpace(spec.operationID) == "" {
		return nil
	}
	requestID, identityDigest, decisionDigest, err := claudePromptAttemptMaterial(sessionID, route, spec)
	if err != nil {
		return err
	}
	attempt, err := turnstatehook.ReadInteractionAttempt(
		stringExtra(c.cfg.Extra, "interaction_dir", "~/.claude/agenthalo-interactions"), requestID,
	)
	if errors.Is(err, turnstatehook.ErrInteractionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if attempt.RequestID != requestID || attempt.LogicalSession != sessionID || attempt.Kind != "prompt" ||
		attempt.IdentityDigest != identityDigest || attempt.DecisionDigest != decisionDigest {
		return turnstatehook.ErrInteractionAttemptConflict
	}
	return turnstatehook.ErrInteractionAlreadyAttempted
}

func (c *Claude) resolveClaudePromptAttempt(attempt turnstatehook.InteractionAttempt) error {
	return c.resolveClaudeInteractionAttempt(attempt)
}

func claudeTranscriptHasToolResult(records []map[string]any, toolUseID string) bool {
	if toolUseID == "" {
		return false
	}
	for _, record := range records {
		if stringAny(record["type"]) != "user" {
			continue
		}
		for _, raw := range listAny(mapAny(record["message"])["content"]) {
			block := mapAny(raw)
			if stringAny(block["type"]) == "tool_result" && stringAny(block["tool_use_id"]) == toolUseID {
				return true
			}
		}
	}
	return false
}

func claudeTranscriptHasInteractionResult(
	records []map[string]any, spec *claudeInteractionAttemptSpec,
) bool {
	if spec == nil || spec.record.ToolUseID == "" {
		return false
	}
	matches := make([]map[string]any, 0, 1)
	for _, record := range records {
		if stringAny(record["type"]) != "user" {
			continue
		}
		for _, raw := range listAny(mapAny(record["message"])["content"]) {
			block := mapAny(raw)
			if stringAny(block["type"]) == "tool_result" &&
				stringAny(block["tool_use_id"]) == spec.record.ToolUseID {
				matches = append(matches, block)
			}
		}
	}
	if len(matches) != 1 {
		return false
	}
	if spec.kind != "question" {
		return true
	}
	block := matches[0]
	if answers, ok := spec.decision.(map[string]QuestionAnswer); ok {
		return claudeQuestionResultMatches(block["content"], spec.record.ToolInput, answers)
	}
	if dismissed, ok := spec.decision.(map[string]bool); ok && dismissed["dismiss"] {
		if boolAny(block["is_error"]) {
			return true
		}
		content, ok := block["content"].(string)
		if !ok {
			return false
		}
		lower := strings.ToLower(content)
		return strings.Contains(lower, "dismiss") || strings.Contains(lower, "declin") ||
			strings.Contains(lower, "cancel") || strings.Contains(lower, "denied")
	}
	return false
}

const claudeQuestionResultPrefix = "Your questions have been answered: "

// claudeQuestionResultMatches verifies the exact question-to-answer mapping
// emitted by Claude Desktop. Looking only for answer substrings is unsafe: two
// answers can be swapped between questions while leaving the same set of
// strings in the tool_result.
func claudeQuestionResultMatches(
	content any, input any, answers map[string]QuestionAnswer,
) bool {
	result, ok := claudeParseQuestionResult(content)
	if !ok {
		return false
	}
	plans, err := claudeQuestionPlans(input, answers)
	if err != nil || len(result) != len(plans) {
		return false
	}
	for _, plan := range plans {
		actual, found := result[plan.question]
		if !found {
			return false
		}
		choices := make([]string, 0, len(plan.selected))
		for _, selected := range plan.selected {
			if claudeAXText(selected) != "other" {
				choices = append(choices, selected)
			}
		}
		plain := append([]string(nil), choices...)
		if plan.other != "" {
			plain = append(plain, plan.other)
		}
		if actual == strings.Join(plain, ", ") {
			continue
		}
		// Claude's stream-json control route historically represents native
		// Other answers as "Other: <text>". Accept that one exact alternate
		// representation, but still require the same question key and reject
		// missing, additional, or swapped answers.
		withOtherLabel := append([]string(nil), choices...)
		if plan.other != "" {
			withOtherLabel = append(withOtherLabel, "Other: "+plan.other)
		}
		if actual != strings.Join(withOtherLabel, ", ") {
			return false
		}
	}
	return true
}

func claudeParseQuestionResult(content any) (map[string]string, bool) {
	text, ok := content.(string)
	if !ok || !strings.HasPrefix(text, claudeQuestionResultPrefix) {
		return nil, false
	}
	rest := strings.TrimPrefix(text, claudeQuestionResultPrefix)
	if rest == "" {
		return nil, false
	}
	result := map[string]string{}
	for {
		question, remainder, ok := claudeConsumeJSONString(rest)
		if !ok || question == "" || !strings.HasPrefix(remainder, "=") {
			return nil, false
		}
		answer, remainder, ok := claudeConsumeJSONString(strings.TrimPrefix(remainder, "="))
		if !ok {
			return nil, false
		}
		if _, duplicate := result[question]; duplicate {
			return nil, false
		}
		result[question] = answer
		if remainder == "" {
			return result, true
		}
		if !strings.HasPrefix(remainder, ", ") {
			return nil, false
		}
		rest = strings.TrimPrefix(remainder, ", ")
		if rest == "" {
			return nil, false
		}
	}
}

func claudeConsumeJSONString(text string) (string, string, bool) {
	if len(text) < 2 || text[0] != '"' {
		return "", text, false
	}
	escaped := false
	for index := 1; index < len(text); index++ {
		switch {
		case escaped:
			escaped = false
		case text[index] == '\\':
			escaped = true
		case text[index] == '"':
			var decoded string
			if err := json.Unmarshal([]byte(text[:index+1]), &decoded); err != nil {
				return "", text, false
			}
			return decoded, text[index+1:], true
		}
	}
	return "", text, false
}

func claudeQuestionPlans(input any, answers map[string]QuestionAnswer) ([]claudeQuestionPlan, error) {
	questions := listAny(mapAny(input)["questions"])
	if len(questions) == 0 || len(questions) != len(answers) {
		return nil, errors.New("Claude question answers do not match the pending request")
	}
	plans := make([]claudeQuestionPlan, 0, len(questions))
	seenQuestions := map[string]bool{}
	for _, raw := range questions {
		question := mapAny(raw)
		text := strings.TrimSpace(stringAny(question["question"]))
		answer, ok := answers[text]
		if text == "" || !ok || seenQuestions[text] {
			return nil, errors.New("Claude question identity is missing or ambiguous")
		}
		seenQuestions[text] = true
		options := map[string]string{}
		for _, optionRaw := range listAny(question["options"]) {
			label := strings.TrimSpace(stringAny(mapAny(optionRaw)["label"]))
			if label == "" || options[claudeAXText(label)] != "" {
				return nil, errors.New("Claude question option identity is ambiguous")
			}
			options[claudeAXText(label)] = label
		}
		selected := make([]string, 0, len(answer.Selected)+1)
		seenSelected := map[string]bool{}
		for _, supplied := range answer.Selected {
			label := options[claudeAXText(supplied)]
			if label == "" || seenSelected[claudeAXText(label)] {
				return nil, errors.New("Claude question selected an unknown or duplicate option")
			}
			seenSelected[claudeAXText(label)] = true
			selected = append(selected, label)
		}
		other := strings.TrimSpace(answer.Other)
		if other != "" && !seenSelected["other"] {
			seenSelected["other"] = true
			selected = append(selected, "Other")
		}
		if len(selected) == 0 || (!boolAny(question["multiSelect"]) && len(selected) != 1) {
			return nil, errors.New("Claude question has an invalid selection count")
		}
		plans = append(plans, claudeQuestionPlan{
			question: text, multi: boolAny(question["multiSelect"]), selected: selected, other: other,
		})
	}
	return plans, nil
}

func claudeQuestionOption(
	snapshot claudeAXSnapshot, question, option string, selected bool, requireState bool,
) (claudeAXElement, error) {
	questionAnchor, err := claudeQuestionAnchor(snapshot, question)
	if err != nil {
		return claudeAXElement{}, err
	}
	wantOption := claudeAXText(option)
	optionAnchors := make([]claudeAXElement, 0, 1)
	bestAnchorScore := -1
	for _, element := range snapshot.elements {
		if claudeAXText(element.Label) != wantOption && claudeAXText(element.Value) != wantOption {
			continue
		}
		score := claudeCommonPathPrefix(questionAnchor.Path, element.Path)
		if score > bestAnchorScore {
			bestAnchorScore = score
			optionAnchors = []claudeAXElement{element}
		} else if score == bestAnchorScore {
			optionAnchors = append(optionAnchors, element)
		}
	}
	if bestAnchorScore <= 0 || len(optionAnchors) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop question option text is missing or ambiguous")
	}
	anchor := optionAnchors[0]
	buttons := make([]claudeAXElement, 0, 1)
	bestButtonDepth := -1
	for _, element := range snapshot.elements {
		roleOK := element.Role == "AXRadioButton" || element.Role == "AXCheckBox" || element.Role == "AXButton"
		enabled := element.Enabled == nil || *element.Enabled
		ancestor := len(element.Path) <= len(anchor.Path)
		if ancestor {
			for index := range element.Path {
				if element.Path[index] != anchor.Path[index] {
					ancestor = false
					break
				}
			}
		}
		if !roleOK || !element.Actionable || !enabled || !ancestor ||
			claudeCommonPathPrefix(questionAnchor.Path, element.Path) <= 0 {
			continue
		}
		if requireState {
			state, known := claudeAXBinaryState(element)
			if !known || state != selected {
				continue
			}
		}
		if len(element.Path) > bestButtonDepth {
			bestButtonDepth = len(element.Path)
			buttons = []claudeAXElement{element}
		} else if len(element.Path) == bestButtonDepth {
			buttons = append(buttons, element)
		}
	}
	if len(buttons) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop question option button is missing or ambiguous")
	}
	return buttons[0], nil
}

func claudeQuestionAnchor(snapshot claudeAXSnapshot, question string) (claudeAXElement, error) {
	want := claudeAXText(question)
	return snapshot.unique(func(element claudeAXElement) bool {
		roleOK := element.Role == "AXStaticText" || element.Role == "AXHeading" || element.Role == "AXText"
		return roleOK && (claudeAXText(element.Label) == want || claudeAXText(element.Value) == want)
	})
}

func claudeQuestionOtherField(snapshot claudeAXSnapshot, question string) (claudeAXElement, error) {
	return claudeUniqueNearAnchor(
		snapshot,
		func(element claudeAXElement) bool {
			return claudeAXText(element.Label) == claudeAXText(question) ||
				claudeAXText(element.Value) == claudeAXText(question)
		},
		func(element claudeAXElement) bool {
			if element.Role != "AXTextField" && element.Role != "AXTextArea" {
				return false
			}
			return claudeAXText(element.Label) == "other option"
		},
	)
}

func claudeSubmitAnswersButton(snapshot claudeAXSnapshot, question string) (claudeAXElement, error) {
	return claudeQuestionNavigationButton(snapshot, question, "Submit")
}

func claudeSubmitAnswersActionable(snapshot claudeAXSnapshot, question string) bool {
	_, err := claudeSubmitAnswersButton(snapshot, question)
	return err == nil
}

func claudeQuestionNavigationButton(snapshot claudeAXSnapshot, question, label string) (claudeAXElement, error) {
	anchor, err := claudeQuestionAnchor(snapshot, question)
	if err != nil {
		return claudeAXElement{}, err
	}
	want := claudeAXText(label)
	bestScore := -1
	best := make([]claudeAXElement, 0, 1)
	for _, element := range snapshot.elements {
		enabled := element.Enabled == nil || *element.Enabled
		if element.Role != "AXButton" || !element.Actionable || !enabled || claudeAXText(element.Label) != want {
			continue
		}
		score := claudeCommonPathPrefix(anchor.Path, element.Path)
		if score > bestScore {
			bestScore = score
			best = []claudeAXElement{element}
		} else if score == bestScore {
			best = append(best, element)
		}
	}
	if bestScore <= 0 || len(best) != 1 {
		return claudeAXElement{}, errors.New("Claude Desktop question navigation is missing or ambiguous")
	}
	return best[0], nil
}

func claudeDismissQuestionButton(snapshot claudeAXSnapshot, question string) (claudeAXElement, error) {
	return claudeQuestionNavigationButton(snapshot, question, "Dismiss question")
}

func claudeQuestionVisible(snapshot claudeAXSnapshot, question string) bool {
	_, err := claudeQuestionAnchor(snapshot, question)
	return err == nil
}

func (c *Claude) claudeComputerUseControl(
	ctx context.Context,
	sessionID string,
	validate func() error,
	attemptSpec *claudeInteractionAttemptSpec,
	operate func(context.Context, *claudeDesktopTransaction, claudeComputerUseDependencies) error,
) claudeComputerUseOutcome {
	outcome := claudeComputerUseOutcome{Disposition: claudeComputerUseNotAttempted}
	handler, deps, done, err := c.beginClaudeComputerUse(sessionID)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	defer done()
	target, err := c.claudeDesktopTarget(sessionID, deps.sessions())
	if err != nil || target.new {
		outcome.Err = errors.New("Claude Desktop control requires an exact existing session")
		return outcome
	}
	if validate != nil {
		if err := validate(); err != nil {
			outcome.Err = err
			return outcome
		}
	}
	deepLink, err := claudeDesktopDeepLink(target)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	appPath := stringExtra(c.cfg.Extra, "desktop_app_path", claudeDesktopDefaultAppPath)
	bundleID := stringExtra(c.cfg.Extra, "desktop_bundle_id", claudeDesktopDefaultBundleID)
	teamID := stringExtra(c.cfg.Extra, "desktop_team_id", claudeDesktopDefaultTeamID)
	if err := deps.verifyApp(ctx, appPath, bundleID, teamID); err != nil {
		outcome.Err = err
		return outcome
	}
	if err := deps.launchApp(ctx, appPath); err != nil {
		outcome.Err = errors.New("Claude Desktop could not start in the background")
		return outcome
	}
	if err := deps.waitApp(ctx, appPath); err != nil {
		outcome.Err = err
		return outcome
	}
	var tx *claudeDesktopTransaction
	var interactionAttempt turnstatehook.InteractionAttempt
	err = handler(ctx, sessionID, func(operationCtx context.Context, tool ComputerUseToolHandler) error {
		tx = &claudeDesktopTransaction{
			tool: tool, bundleID: bundleID, target: &target, validate: validate,
		}
		initial, inspectErr := tx.inspect(operationCtx)
		if claudeComputerUseSecurityRefusal(inspectErr) {
			tx.noFallback = true
			return inspectErr
		}
		if inspectErr != nil || len(initial.elements) == 0 {
			return errors.New("Claude Desktop exposed no Accessibility surface")
		}
		if err := c.commitClaudeControlRoute(
			operationCtx, sessionID, claudeRouteDesktopComputerUse,
		); err != nil {
			tx.noFallback = true
			return errors.New("Claude Desktop route could not be committed")
		}
		interactionAttempt, err = c.beginClaudeInteractionAttempt(sessionID, attemptSpec)
		if err != nil {
			tx.noFallback = true
			return errors.New("Claude Desktop interaction was already attempted or could not be recorded")
		}
		tx.mutated = true
		if err := deps.openURL(operationCtx, appPath, deepLink); err != nil {
			return errors.New("Claude Desktop could not open the target surface")
		}
		for {
			snapshot, readyErr := tx.inspect(operationCtx)
			if readyErr != nil {
				return readyErr
			}
			if tx.verifyTarget(snapshot) == nil {
				break
			}
			if err := deps.sleep(operationCtx, 100*time.Millisecond); err != nil {
				return err
			}
		}
		return operate(operationCtx, tx, deps)
	})
	if tx != nil && (tx.mutated || tx.noFallback) {
		c.bindClaudeComputerUseRoute(sessionID, claudeRouteDesktopComputerUse)
	}
	if err != nil {
		outcome.Err = err
		if claudeComputerUseSecurityRefusal(err) || tx != nil && (tx.mutated || tx.noFallback) {
			outcome.Disposition = claudeComputerUseDeliveryUnknown
		}
		return outcome
	}
	if tx == nil || !tx.confirmed {
		outcome.Err = errors.New("Claude Desktop control postcondition was not confirmed")
		if tx != nil && tx.mutated {
			outcome.Disposition = claudeComputerUseDeliveryUnknown
		}
		return outcome
	}
	if attemptSpec != nil {
		// The automation callback has returned, so the server has synchronously
		// closed/relocked its short window. Transcript polling below is pure file
		// observation and never keeps the desktop unlocked while a tool runs.
		confirmed := false
		for {
			if claudeTranscriptHasInteractionResult(deps.records(target.cliID), attemptSpec) {
				confirmed = true
				break
			}
			if err := deps.sleep(ctx, 100*time.Millisecond); err != nil {
				break
			}
		}
		if !confirmed || c.resolveClaudeInteractionAttempt(interactionAttempt) != nil {
			outcome.Err = errors.New("Claude Desktop interaction result could not be confirmed")
			outcome.Disposition = claudeComputerUseDeliveryUnknown
			return outcome
		}
	}
	outcome.Disposition = claudeComputerUseConfirmed
	outcome.DesktopSessionID = target.desktopID
	outcome.TranscriptID = target.cliID
	return outcome
}

func (c *Claude) claudeComputerUseRelayApproval(
	ctx context.Context, sessionID string, record turnstatehook.InteractionRecord, decision string,
) claudeComputerUseOutcome {
	validate := func() error {
		current, err := c.claudeDesktopInteractionRaw(sessionID, record.RequestID, record.ToolName)
		if err != nil || current.ToolUseID != record.ToolUseID {
			return errors.New("Claude permission request is no longer exact and pending")
		}
		return nil
	}
	return c.claudeComputerUseControl(ctx, sessionID, validate, &claudeInteractionAttemptSpec{
		record: record, kind: "permission", decision: map[string]string{"decision": decision},
	}, func(
		operationCtx context.Context, tx *claudeDesktopTransaction, deps claudeComputerUseDependencies,
	) error {
		if err := tx.press(operationCtx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
			return claudeApprovalButton(snapshot, record, decision)
		}); err != nil {
			return err
		}
		snapshot, err := tx.inspect(operationCtx)
		if err == nil && tx.verifyTarget(snapshot) == nil &&
			!claudeApprovalCardActionable(snapshot, record) {
			tx.confirmed = true
			return nil
		}
		return errors.New("Claude Desktop permission decision could not be confirmed")
	})
}

func (c *Claude) claudeComputerUseAnswerQuestion(
	ctx context.Context,
	sessionID string,
	record turnstatehook.InteractionRecord,
	answers map[string]QuestionAnswer,
) claudeComputerUseOutcome {
	plans, err := claudeQuestionPlans(record.ToolInput, answers)
	if err != nil {
		return claudeComputerUseOutcome{Disposition: claudeComputerUseNotAttempted, Err: err}
	}
	validate := func() error {
		current, err := c.claudeDesktopInteractionRaw(sessionID, record.RequestID, "AskUserQuestion")
		if err != nil || current.ToolUseID != record.ToolUseID {
			return errors.New("Claude question request is no longer exact and pending")
		}
		return nil
	}
	return c.claudeComputerUseControl(ctx, sessionID, validate, &claudeInteractionAttemptSpec{
		record: record, kind: "question", decision: answers,
	}, func(
		operationCtx context.Context, tx *claudeDesktopTransaction, _ claudeComputerUseDependencies,
	) error {
		return claudeOperateQuestionAnswers(operationCtx, tx, plans)
	})
}

func claudeOperateQuestionAnswers(
	ctx context.Context, tx *claudeDesktopTransaction, plans []claudeQuestionPlan,
) error {
	if tx == nil || len(plans) == 0 {
		return errors.New("Claude Desktop question plan is empty")
	}
	for planIndex, plan := range plans {
		for _, option := range plan.selected {
			selectedOption := option
			if err := tx.press(ctx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
				return claudeQuestionOption(snapshot, plan.question, selectedOption, false, plan.multi)
			}); err != nil {
				return err
			}
			snapshot, err := tx.inspect(ctx)
			if err != nil || tx.verifyTarget(snapshot) != nil {
				return errors.New("Claude Desktop option selection could not be verified")
			}
			if plan.multi {
				if _, err := claudeQuestionOption(snapshot, plan.question, selectedOption, true, true); err != nil {
					return errors.New("Claude Desktop did not retain the exact option selection")
				}
			} else if !(claudeAXText(selectedOption) == "other" && plan.other != "") {
				navigation := "Submit"
				if planIndex < len(plans)-1 {
					navigation = "Next"
				}
				if _, err := claudeQuestionNavigationButton(snapshot, plan.question, navigation); err != nil {
					return errors.New("Claude Desktop single-select choice did not enable exact navigation")
				}
			}
		}
		if plan.other != "" {
			if err := tx.setValue(ctx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
				return claudeQuestionOtherField(snapshot, plan.question)
			}, plan.other); err != nil {
				return err
			}
			fresh, err := tx.inspect(ctx)
			if err != nil || tx.verifyTarget(fresh) != nil {
				return errors.New("Claude Desktop Other answer could not be verified")
			}
			field, err := claudeQuestionOtherField(fresh, plan.question)
			if err != nil || field.Value != plan.other {
				return errors.New("Claude Desktop did not retain the exact Other answer")
			}
			navigation := "Submit"
			if planIndex < len(plans)-1 {
				navigation = "Next"
			}
			if _, err := claudeQuestionNavigationButton(fresh, plan.question, navigation); err != nil {
				return errors.New("Claude Desktop Other answer did not enable exact navigation")
			}
		}
		if planIndex < len(plans)-1 {
			if err := tx.press(ctx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
				return claudeQuestionNavigationButton(snapshot, plan.question, "Next")
			}); err != nil {
				return err
			}
			next, err := tx.inspect(ctx)
			if err != nil || tx.verifyTarget(next) != nil || !claudeQuestionVisible(next, plans[planIndex+1].question) {
				return errors.New("Claude Desktop did not advance to the exact next question")
			}
		}
	}
	lastQuestion := plans[len(plans)-1].question
	if err := tx.press(ctx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
		return claudeSubmitAnswersButton(snapshot, lastQuestion)
	}); err != nil {
		return err
	}
	snapshot, err := tx.inspect(ctx)
	if err == nil && tx.verifyTarget(snapshot) == nil && !claudeSubmitAnswersActionable(snapshot, lastQuestion) {
		tx.confirmed = true
		return nil
	}
	return errors.New("Claude Desktop question answers could not be confirmed")
}

func (c *Claude) claudeComputerUseDismissQuestion(
	ctx context.Context, sessionID string, record turnstatehook.InteractionRecord,
) claudeComputerUseOutcome {
	questions := listAny(mapAny(record.ToolInput)["questions"])
	if len(questions) == 0 {
		return claudeComputerUseOutcome{
			Disposition: claudeComputerUseNotAttempted, Err: errors.New("Claude question payload is empty"),
		}
	}
	question := strings.TrimSpace(stringAny(mapAny(questions[0])["question"]))
	if question == "" {
		return claudeComputerUseOutcome{
			Disposition: claudeComputerUseNotAttempted, Err: errors.New("Claude question identity is missing"),
		}
	}
	validate := func() error {
		current, err := c.claudeDesktopInteractionRaw(sessionID, record.RequestID, "AskUserQuestion")
		if err != nil || current.ToolUseID != record.ToolUseID {
			return errors.New("Claude question request is no longer exact and pending")
		}
		return nil
	}
	return c.claudeComputerUseControl(ctx, sessionID, validate, &claudeInteractionAttemptSpec{
		record: record, kind: "question", decision: map[string]bool{"dismiss": true},
	}, func(
		operationCtx context.Context, tx *claudeDesktopTransaction, _ claudeComputerUseDependencies,
	) error {
		if err := tx.press(operationCtx, func(snapshot claudeAXSnapshot) (claudeAXElement, error) {
			return claudeDismissQuestionButton(snapshot, question)
		}); err != nil {
			return err
		}
		snapshot, err := tx.inspect(operationCtx)
		if err != nil || tx.verifyTarget(snapshot) != nil {
			return errors.New("Claude Desktop question dismissal could not be confirmed")
		}
		if _, err := claudeDismissQuestionButton(snapshot, question); err == nil {
			return errors.New("Claude Desktop question dismissal is still actionable")
		}
		tx.confirmed = true
		return nil
	})
}
