package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

const (
	defaultPreviewTail = 60
	fileMaxBytes       = 1024 * 1024
	imageMaxBytes      = 512 * 1024
	projectMaxEntries  = 600
	dirBrowseMax       = 400
	gitPatchMaxChars   = 700000
)

var (
	projectSkipDirs = map[string]bool{
		".git": true, ".hg": true, ".svn": true, ".venv": true, "venv": true,
		"node_modules": true, "Pods": true, ".gradle": true, "build": true,
		"dist": true, ".next": true, ".turbo": true, "__pycache__": true,
	}
	imageMime = map[string]string{
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml",
	}
	lineSuffixRE = regexp.MustCompile(`:\d+(?::\d+)?$`)
	commitRE     = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)
	providerIDRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

func (s *Server) getProvider(id string) (provider.Provider, string, bool) {
	if id == "" {
		s.mu.Lock()
		id = s.activeProvider
		s.mu.Unlock()
	}
	id = canonicalProviderID(id)
	p, ok := s.registry[id]
	return p, id, ok
}

func canonicalProviderID(id string) string {
	switch id {
	case "claude_cli", "claude_desktop":
		return "claude"
	default:
		return id
	}
}

func sameProviderID(a string, b string) bool {
	return canonicalProviderID(a) == canonicalProviderID(b)
}

func cleanOptional(v string) string {
	v = strings.TrimSpace(v)
	return v
}

func normalizeCwd(cwd string) (string, error) {
	cwd = cleanOptional(cwd)
	if cwd == "" {
		return "", nil
	}
	rp, err := realpath(expandUser(cwd))
	if err != nil {
		return "", err
	}
	st, err := os.Stat(rp)
	if err != nil || !st.IsDir() {
		return "", errors.New("cwd is not a directory: " + cwd)
	}
	return rp, nil
}

func validateStartOptions(p provider.Provider, in createSessionIn) (provider.StartOptions, error) {
	opts := provider.StartOptions{}
	cwd, err := normalizeCwd(in.Cwd)
	if err != nil {
		return opts, err
	}
	ms := p.ModelSelect()
	model := cleanOptional(in.Model)
	effort := cleanOptional(in.Effort)
	mode := cleanOptional(in.Mode)
	if model != "" && len(ms.Models) > 0 && !modelAllowed(ms.Models, model) {
		return opts, errors.New("unknown model: " + model)
	}
	if effort != "" && len(ms.Efforts) > 0 && !stringIn(ms.Efforts, effort) {
		return opts, errors.New("unknown effort: " + effort)
	}
	modeIDs := []string{"auto", "edit", "plan", "default"}
	if len(ms.Modes) > 0 {
		modeIDs = modeIDs[:0]
		for _, m := range ms.Modes {
			modeIDs = append(modeIDs, m.ID)
		}
	}
	if mode != "" && !stringIn(modeIDs, mode) {
		return opts, errors.New("unknown mode: " + mode)
	}
	opts.Cwd, opts.Model, opts.Effort, opts.Mode = cwd, model, effort, mode
	return opts, nil
}

func modelAllowed(models []provider.ModelOption, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

func stringIn(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")[:12]
	}
	return hex.EncodeToString(b[:])
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newSessionRecord(deviceID string, providerID string, title string, opts provider.StartOptions) state.Record {
	if title == "" {
		title = "untitled"
	}
	ts := nowISO()
	return state.Record{
		"session_id":           newID(),
		"device_id":            deviceID,
		"provider_id":          providerID,
		"title":                title,
		"cwd":                  opts.Cwd,
		"model":                opts.Model,
		"effort":               opts.Effort,
		"mode":                 opts.Mode,
		"state":                "idle",
		"native_session_id":    nil,
		"transcript_id":        nil,
		"last_prompt":          "",
		"last_response":        "",
		"last_screenshot_path": "",
		"last_error":           "",
		"created_at":           ts,
		"updated_at":           ts,
	}
}

func newTaskRecord(deviceID string, sessionID string, providerID string, prompt string) state.Record {
	ts := nowISO()
	return state.Record{
		"task_id":          newID(),
		"session_id":       sessionID,
		"device_id":        deviceID,
		"provider_id":      providerID,
		"native_task_id":   nil,
		"prompt":           prompt,
		"status":           "queued",
		"response":         "",
		"screenshot_path":  "",
		"approval_request": nil,
		"error":            "",
		"created_at":       ts,
		"updated_at":       ts,
	}
}

func (s *Server) findSessionAny(id string) (state.Record, bool, error) {
	return s.findSessionForProviderAny("", id)
}

// findSessionForProviderAny resolves every public session identifier while
// keeping provider ownership part of the lookup. Native Claude transcript IDs
// can legitimately be visible to both the Desktop and CLI providers, so an
// unscoped lookup is unsafe for delivery/control operations.
func (s *Server) findSessionForProviderAny(providerID string, id string) (state.Record, bool, error) {
	records, err := s.store.Sessions()
	if err != nil {
		return nil, false, err
	}
	for _, r := range records {
		if providerID != "" && !sameProviderID(recordString(r, "provider_id"), providerID) {
			continue
		}
		if recordString(r, "session_id") == id ||
			recordString(r, "native_session_id") == id ||
			recordString(r, "transcript_id") == id {
			return r, true, nil
		}
	}
	return nil, false, nil
}

func providerRuntimeSession(p provider.Provider, id string) (map[string]any, bool) {
	if id == "" {
		return nil, false
	}
	if runtime, ok := p.(interface{ RuntimeSessions() []map[string]any }); ok {
		for _, row := range runtime.RuntimeSessions() {
			if id == stringAny(row["session_id"]) || id == stringAny(row["transcript_id"]) ||
				id == stringAny(row["native_session_id"]) || id == stringAny(row["cli_session_id"]) {
				return row, true
			}
		}
	}
	return nil, false
}

// hydrateControlSession restores the provider's in-memory logical->native
// mapping before every mutating control operation. It also rejects IDs owned
// by another provider instead of letting providers interpret an arbitrary
// logical ID as a native thread/transcript.
func (s *Server) hydrateControlSession(p provider.Provider, providerID string, id string) error {
	if err := rejectUnsafeSessionID(id); err != nil {
		return err
	}
	if rec, ok, err := s.findSessionForProviderAny(providerID, id); err != nil {
		return err
	} else if ok {
		logical := recordString(rec, "session_id")
		transcript := firstNonEmpty(recordString(rec, "transcript_id"), recordString(rec, "native_session_id"))
		if logical != "" && transcript != "" {
			bindSessionTranscript(p, rec, logical, transcript)
			bindSessionTranscript(p, rec, id, transcript)
		}
		return nil
	}
	if row, ok := providerRuntimeSession(p, id); ok {
		transcript := firstNonEmpty(stringAny(row["transcript_id"]), firstNonEmpty(stringAny(row["cli_session_id"]), stringAny(row["native_session_id"])))
		if transcript == "" {
			transcript = id
		}
		if binder, ok := p.(interface{ BindTranscript(string, string) }); ok {
			binder.BindTranscript(id, transcript)
		}
		return nil
	}
	// A provider-reported pending session is authoritative even in the small
	// race before RuntimeSessions publishes its row. Do not infer ownership by
	// parsing a transcript: the same native transcript can be visible through
	// both Claude Desktop and claude_cli.
	if pending, ok := p.(interface{ PendingApprovalSessionIDs() []string }); ok {
		for _, pendingID := range pending.PendingApprovalSessionIDs() {
			if pendingID != id {
				continue
			}
			if binder, ok := p.(interface{ BindTranscript(string, string) }); ok {
				binder.BindTranscript(id, id)
			}
			return nil
		}
	}
	return errors.New("unknown session_id for provider " + providerID + ": " + id)
}

func (s *Server) bindProviderTranscript(p provider.Provider, id string) {
	if id == "" {
		return
	}
	if _, ok := p.(interface{ BindTranscript(string, string) }); !ok {
		return
	}
	rec, ok, err := s.findSessionForProviderAny(p.ID(), id)
	if err != nil {
		return
	}
	if !ok {
		// Native-session previews are intentionally not persisted as logical
		// sessions until the web sends a prompt. Bind the native id to itself so
		// providers can subscribe/refresh Desktop-owned pending requests while
		// the tab is still read-only.
		p.(interface{ BindTranscript(string, string) }).BindTranscript(id, id)
		return
	}
	sessionID := recordString(rec, "session_id")
	transcriptID := recordString(rec, "transcript_id")
	if sessionID != "" && transcriptID != "" {
		bindSessionTranscript(p, rec, sessionID, transcriptID)
	}
	if id != "" && transcriptID != "" {
		bindSessionTranscript(p, rec, id, transcriptID)
	}
}

func bindSessionTranscript(p provider.Provider, rec state.Record, sessionID string, transcriptID string) {
	if p == nil || sessionID == "" || transcriptID == "" {
		return
	}
	if codexDesktopDeliveryRecord(rec) {
		if binder, ok := p.(interface{ BindDesktopTranscript(string, string) }); ok {
			binder.BindDesktopTranscript(sessionID, transcriptID)
			return
		}
	}
	if binder, ok := p.(interface{ BindTranscript(string, string) }); ok {
		binder.BindTranscript(sessionID, transcriptID)
	}
}

func codexDesktopDeliveryRecord(rec state.Record) bool {
	if canonicalProviderID(recordString(rec, "provider_id")) != "codex" {
		return false
	}
	if recordString(rec, "delivery_route") == "desktop_ipc" {
		return true
	}
	// Compatibility for native Desktop sessions persisted before the route was
	// explicit. app-server-owned logical ids are random hex; the historical
	// Desktop activation ids use r<thread-prefix>, while new ones use r-codex-.
	sessionID := recordString(rec, "session_id")
	return strings.HasPrefix(sessionID, "r0") || strings.HasPrefix(sessionID, "r-codex-")
}

// providerScopedLogicalID makes new persisted IDs unique across providers.
// Existing stored IDs remain valid and are reused by resumeNativeSession.
func providerScopedLogicalID(providerID string, nativeID string) string {
	providerID = canonicalProviderID(providerID)
	sum := sha256.Sum256([]byte(providerID + "\x00" + nativeID))
	name := providerIDRE.ReplaceAllString(providerID, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "provider"
	}
	return "r-" + name + "-" + hex.EncodeToString(sum[:6])
}

func recordString(r state.Record, key string) string {
	if v, ok := r[key].(string); ok {
		return v
	}
	return ""
}

func expandUser(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func realpath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func gitTopLevel(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	rp, err := realpath(root)
	if err != nil {
		return ""
	}
	return rp
}

func addProjectRoot(rows *[]map[string]any, seen map[string]bool, path string, source string) {
	if path == "" {
		return
	}
	rp, err := realpath(expandUser(path))
	if err != nil {
		return
	}
	st, err := os.Stat(rp)
	if err != nil || !st.IsDir() {
		return
	}
	if top := gitTopLevel(rp); top != "" {
		rp = top
	}
	if seen[rp] {
		return
	}
	seen[rp] = true
	*rows = append(*rows, map[string]any{"path": rp, "name": filepath.Base(rp), "source": source})
}

func (s *Server) browseRoots() []string {
	roots := []string{}
	add := func(p string) {
		if p == "" {
			return
		}
		rp, err := realpath(expandUser(p))
		if err != nil {
			return
		}
		st, err := os.Stat(rp)
		if err == nil && st.IsDir() && !stringIn(roots, rp) {
			roots = append(roots, rp)
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		add(h)
	}
	for _, p := range []string{"~/Developer", "~/Developer/Projects", "~/Projects", "~/Documents", "~/Desktop"} {
		add(p)
	}
	for _, p := range s.cfg.ProjectRoots {
		add(p)
	}
	for _, id := range s.registry.IDs() {
		add(s.registry[id].Status().Cwd)
	}
	return roots
}

func (s *Server) safeBrowseDir(path string) (string, []string, int, string) {
	roots := s.browseRoots()
	if len(roots) == 0 {
		return "", nil, http.StatusNotFound, "no browsable roots"
	}
	target := roots[0]
	if path != "" {
		target = path
	}
	rp, err := realpath(expandUser(target))
	if err != nil {
		return "", roots, http.StatusNotFound, "directory not found"
	}
	allowed := false
	for _, r := range roots {
		if under(rp, r) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", roots, http.StatusForbidden, "path outside browsable roots"
	}
	st, err := os.Stat(rp)
	if err != nil || !st.IsDir() {
		return "", roots, http.StatusNotFound, "directory not found"
	}
	return rp, roots, 0, ""
}

func rejectUnsafeSessionID(id string) error {
	if id == "" {
		return errors.New("invalid session_id")
	}
	if strings.Contains(id, "/") || strings.Contains(id, `\`) || strings.Contains(id, "..") ||
		strings.Contains(id, "\x00") || strings.ContainsAny(id, "*?[") {
		return errors.New("invalid session_id")
	}
	return nil
}

func (s *Server) sessionProjectRoot(providerID, sessionID string) (string, error) {
	if err := rejectUnsafeSessionID(sessionID); err != nil {
		return "", err
	}
	cwd := ""
	if p, resolvedProviderID, ok := s.getProvider(providerID); ok {
		providerID = resolvedProviderID
		nativeRows, _ := s.nativeSessionsForProvider(resolvedProviderID, p, true)
		for _, row := range nativeRows {
			if sessionID == stringAny(row["native_session_id"]) || sessionID == stringAny(row["cli_session_id"]) {
				cwd = stringAny(row["cwd"], row["worktree"])
				break
			}
		}
	}
	if cwd == "" {
		rec, ok, err := s.findSessionForProviderAny(providerID, sessionID)
		if err != nil {
			return "", err
		}
		if ok {
			cwd = recordString(rec, "cwd")
		}
	}
	if cwd == "" {
		return "", errors.New("session cwd not found")
	}
	rp, err := realpath(expandUser(cwd))
	if err != nil {
		return "", errors.New("project root not found")
	}
	st, err := os.Stat(rp)
	if err != nil || !st.IsDir() {
		return "", errors.New("project root not found")
	}
	if top := gitTopLevel(rp); top != "" {
		return top, nil
	}
	return rp, nil
}

func projectPath(root, rel string) (string, error) {
	if strings.Contains(rel, "\x00") {
		return "", errors.New("invalid path")
	}
	if rel == "" {
		rel = "."
	}
	rel = lineSuffixRE.ReplaceAllString(rel, "")
	var target string
	if filepath.IsAbs(rel) {
		target = expandUser(rel)
	} else {
		target = filepath.Join(root, rel)
	}
	rp, err := realpath(target)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(rel) && rp != root && !under(rp, root) {
		rootName := filepath.Base(root)
		parts := strings.Split(filepath.Clean(expandUser(rel)), string(os.PathSeparator))
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != rootName {
				continue
			}
			candidate, err := realpath(filepath.Join(append([]string{root}, parts[i+1:]...)...))
			if err == nil && (candidate == root || under(candidate, root)) {
				rp = candidate
				break
			}
		}
	}
	if rp != root && !under(rp, root) {
		return "", errors.New("path outside project")
	}
	return rp, nil
}

func sortDirEntries(entries []map[string]any) {
	sort.Slice(entries, func(i, j int) bool {
		ti, _ := entries[i]["type"].(string)
		tj, _ := entries[j]["type"].(string)
		if ti != tj {
			return ti == "dir"
		}
		ni, _ := entries[i]["name"].(string)
		nj, _ := entries[j]["name"].(string)
		return strings.ToLower(ni) < strings.ToLower(nj)
	})
}
