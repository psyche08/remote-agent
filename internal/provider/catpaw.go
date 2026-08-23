package provider

import (
	"bytes"
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

	"github.com/psyche08/remote-agent/internal/config"
)

const (
	catPawBundleID = "com.meituan.catpaw"
	catPawTeamID   = "BHWTW6L8X6"

	catPawDefaultAppPath = "~/Applications/CatPaw.app"
	catPawDefaultDBPath  = "~/.sankuai/MCopilot/sqliteDB/globalCache.sqlite"
	catPawSQLitePath     = "/usr/bin/sqlite3"

	catPawQueryTimeout       = 3 * time.Second
	catPawSignatureCacheTTL  = 15 * time.Second
	catPawSessionLimit       = 200
	catPawMessageLimit       = 800
	catPawMaxQueryOutput     = 24 * 1024 * 1024
	catPawMaxMessageJSONSize = 16 * 1024 * 1024
)

var (
	errCatPawReadOnly  = errors.New("CatPaw provider is read-only")
	errCatPawActiveWAL = errors.New(
		"CatPaw history has an active WAL; immutable read is stale and was refused",
	)
	errCatPawActiveJournal = errors.New(
		"CatPaw history has an active rollback journal; immutable read was refused",
	)
	errCatPawDatabaseChanged = errors.New(
		"CatPaw history changed during immutable read; stale snapshot was refused",
	)
)

type catPawSQLiteQueryer interface {
	Query(context.Context, string, string) ([]map[string]any, error)
}

// catPawSQLiteCLI deliberately opens only an immutable URI. It never asks the
// native database for a write lock and never creates a journal or shared-memory
// file. An active WAL is refused because immutable SQLite readers ignore it.
type catPawSQLiteCLI struct {
	command string
}

func (q catPawSQLiteCLI) Query(ctx context.Context, dbPath string, query string) ([]map[string]any, error) {
	before, err := catPawImmutablePreflight(dbPath)
	if err != nil {
		return nil, err
	}
	dbURI := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&immutable=1"}).String()
	var stdout catPawLimitedBuffer
	stdout.limit = catPawMaxQueryOutput
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, q.command, "-json", dbURI, "PRAGMA query_only=ON; "+query)
	cmd.Stdin = nil
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("CatPaw history query timed out")
		}
		return nil, fmt.Errorf("CatPaw history query failed: %w", err)
	}
	if stdout.overflow {
		return nil, errors.New("CatPaw history query exceeded the output limit")
	}
	if err := catPawImmutablePostflight(dbPath, before); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(stdout.buf.Bytes())) == 0 {
		return []map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.buf.Bytes()))
	decoder.UseNumber()
	var rows []map[string]any
	if err := decoder.Decode(&rows); err != nil {
		return nil, errors.New("CatPaw history query returned invalid JSON")
	}
	return rows, nil
}

type catPawLimitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *catPawLimitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.overflow = true
		return n, nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.overflow = true
		return n, nil
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

type catPawDBFingerprint struct {
	size    int64
	modTime time.Time
	info    os.FileInfo
}

func catPawImmutablePreflight(dbPath string) (catPawDBFingerprint, error) {
	st, err := os.Lstat(dbPath)
	if err != nil {
		return catPawDBFingerprint{}, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return catPawDBFingerprint{}, errors.New("CatPaw history path is a symbolic link")
	}
	if !st.Mode().IsRegular() {
		return catPawDBFingerprint{}, errors.New("CatPaw history path is not a regular file")
	}
	if catPawWALActive(dbPath) {
		return catPawDBFingerprint{}, errCatPawActiveWAL
	}
	if catPawRollbackJournalActive(dbPath) {
		return catPawDBFingerprint{}, errCatPawActiveJournal
	}
	return catPawDBFingerprint{size: st.Size(), modTime: st.ModTime(), info: st}, nil
}

func catPawImmutablePostflight(dbPath string, before catPawDBFingerprint) error {
	if catPawWALActive(dbPath) {
		return errCatPawActiveWAL
	}
	if catPawRollbackJournalActive(dbPath) {
		return errCatPawActiveJournal
	}
	st, err := os.Lstat(dbPath)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() ||
		!os.SameFile(before.info, st) ||
		st.Size() != before.size || !st.ModTime().Equal(before.modTime) {
		return errCatPawDatabaseChanged
	}
	return nil
}

// catPawWithImmutableSnapshot extends the per-query immutable checks across a
// complete logical read. Schema discovery and the subsequent data query use
// separate sqlite3 processes, so a writer could otherwise commit between them
// and make one response combine two database generations. The queries remain
// lock-free and read-only; if the backing file changes anywhere in the logical
// read, the entire result is discarded.
func catPawWithImmutableSnapshot[T any](
	dbPath string, read func() (T, error),
) (T, error) {
	var zero T
	before, err := catPawImmutablePreflight(dbPath)
	if err != nil {
		return zero, err
	}
	value, readErr := read()
	snapshotErr := catPawImmutablePostflight(dbPath, before)
	if readErr != nil {
		if snapshotErr != nil {
			return zero, errors.Join(readErr, snapshotErr)
		}
		return zero, readErr
	}
	if snapshotErr != nil {
		return zero, snapshotErr
	}
	return value, nil
}

func catPawWALActive(dbPath string) bool {
	st, err := os.Stat(dbPath + "-wal")
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

func catPawRollbackJournalActive(dbPath string) bool {
	st, err := os.Stat(dbPath + "-journal")
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

type catPawSignatureProbe func(context.Context, string) error

// CatPaw is the phase-one, read-only adapter for the local Meituan CatPaw
// desktop application. It can discover and preview native transcripts, but it
// grants no Computer Use, Locked Use, approval, or prompt-delivery authority.
type CatPaw struct {
	id       string
	appName  string
	appPath  string
	dbPath   string
	queryer  catPawSQLiteQueryer
	sigProbe catPawSignatureProbe

	mu              sync.Mutex
	sigCheckedAt    time.Time
	sigErr          error
	lastHistoryErr  error
	lastHistoryRead time.Time
}

func NewCatPaw(id string, cfg config.ProviderConfig) *CatPaw {
	return &CatPaw{
		id:       firstNonEmpty(id, "catpaw"),
		appName:  firstNonEmpty(cfg.AppName, "CatPaw"),
		appPath:  catPawAbsolutePath(stringExtra(cfg.Extra, "app_path", catPawDefaultAppPath)),
		dbPath:   catPawAbsolutePath(stringExtra(cfg.Extra, "history_db_path", catPawDefaultDBPath)),
		queryer:  catPawSQLiteCLI{command: catPawSQLitePath},
		sigProbe: verifyCatPawSignature,
	}
}

func catPawAbsolutePath(path string) string {
	path = expandUser(path)
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return path
}

func (c *CatPaw) ID() string { return c.id }

func (c *CatPaw) ProviderProfile() ProviderProfile {
	return ProviderProfile{
		ProviderID:       c.id,
		Family:           ProviderFamilyCatPaw,
		AdapterKind:      AdapterKindDesktopTranscript,
		RuntimeNamespace: RuntimeNamespaceCatPaw,
		Surface:          ProviderSurfaceCatPaw,
		Routes: []ProviderRoute{{
			AdapterKind: AdapterKindDesktopTranscript,
			Surface:     ProviderSurfaceCatPaw,
			Role:        ProviderRoutePrimary,
		}},
	}
}

// SupportsAction is intentionally false for every phase-one operation. The
// method is kept separate from legacy capability booleans so typed action
// renderers cannot mistake the Provider interface's fail-closed stubs for
// actual support.
func (c *CatPaw) SupportsAction(ActionID) bool { return false }

func (c *CatPaw) Installed() bool {
	st, err := os.Stat(c.appPath)
	return err == nil && st.IsDir()
}

func (c *CatPaw) Status() Status {
	installed := c.Installed()
	signatureValid := false
	var signatureErr error
	if installed {
		signatureErr = c.signatureError()
		signatureValid = signatureErr == nil
	}
	c.mu.Lock()
	historyErr := c.lastHistoryErr
	c.mu.Unlock()

	state := "idle"
	var lastError string
	switch {
	case !installed:
		lastError = "CatPaw.app is not installed at the configured path"
	case signatureErr != nil:
		state = "error"
		lastError = "CatPaw code signature integrity or identity verification failed"
	case historyErr != nil:
		state = "error"
		lastError = historyErr.Error()
	}
	var errPtr *string
	if lastError != "" {
		errPtr = &lastError
	}
	dbFingerprint, dbErr := catPawImmutablePreflight(c.dbPath)
	historyAvailable := dbErr == nil
	historyPrivate := historyAvailable && dbFingerprint.info.Mode().Perm()&0o077 == 0
	if historyErr == nil && dbErr != nil && !os.IsNotExist(dbErr) {
		historyErr = dbErr
		if state == "idle" {
			state = "error"
			lastError = dbErr.Error()
			errPtr = &lastError
		}
	}
	return Status{
		ProviderID:  c.id,
		AppName:     c.appName,
		IsRunning:   false,
		IsFrontmost: false,
		Installed:   installed,
		State:       state,
		LastError:   errPtr,
		Capabilities: map[string]bool{
			"native_sessions":    historyAvailable,
			"native_task_status": false,
			"desktop_transcript": historyAvailable,
			"history_db_private": historyPrivate,
			"computer_use":       false,
			"locked_use":         false,
			"signature_valid":    signatureValid,
			"approval":           false,
			"interrupt":          false,
			"steer":              false,
			"streaming":          false,
			"create_session":     false,
			"attachments":        false,
			"raw_keys":           false,
		},
		Backend: "catpaw_desktop_transcript_readonly",
		Command: c.appPath,
	}
}

func (c *CatPaw) signatureError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sigCheckedAt.IsZero() && time.Since(c.sigCheckedAt) < catPawSignatureCacheTTL {
		return c.sigErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), catPawQueryTimeout)
	defer cancel()
	c.sigErr = c.sigProbe(ctx, c.appPath)
	c.sigCheckedAt = time.Now()
	return c.sigErr
}

// verifyCatPawSignature is a status/readiness diagnostic for the read-only
// phase. A future mutable Computer Use transaction must perform a fresh exact
// requirement check inside that transaction and must not rely on this cache.
func verifyCatPawSignature(ctx context.Context, appPath string) error {
	verify := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", appPath)
	verify.Stdin = nil
	if err := verify.Run(); err != nil {
		return errors.New("code signature is invalid")
	}
	inspect := exec.CommandContext(ctx, "/usr/bin/codesign", "-dv", "--verbose=4", appPath)
	inspect.Stdin = nil
	out, err := inspect.CombinedOutput()
	if err != nil {
		return errors.New("code signature identity is unavailable")
	}
	return validateCatPawSignatureIdentity(out)
}

func validateCatPawSignatureIdentity(out []byte) error {
	identifier := ""
	teamID := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Identifier="):
			identifier = strings.TrimPrefix(line, "Identifier=")
		case strings.HasPrefix(line, "TeamIdentifier="):
			teamID = strings.TrimPrefix(line, "TeamIdentifier=")
		}
	}
	if identifier != catPawBundleID || teamID != catPawTeamID {
		return errors.New("code signature identity does not match CatPaw")
	}
	return nil
}

func (c *CatPaw) ModelSelect() ModelSelect {
	return ModelSelect{Note: "CatPaw transcript discovery is read-only; model and mode controls are disabled"}
}

type catPawSchema struct {
	tables map[string]map[string]bool
}

func (s catPawSchema) hasTable(name string) bool { return s.tables[name] != nil }

func (s catPawSchema) hasColumn(table string, column string) bool {
	return s.tables[table] != nil && s.tables[table][column]
}

func (s catPawSchema) supportsCurrent() bool {
	return s.hasColumn("history_preview_record_ide", "conversationId") &&
		s.hasColumn("history_detail_record_ide", "conversationId") &&
		s.hasColumn("history_detail_record_ide", "agentMessages")
}

func (s catPawSchema) supportsLegacy() bool {
	return s.hasColumn("t_conversation", "conversation_id") &&
		s.hasColumn("t_conversation", "messages")
}

func (c *CatPaw) discoverSchema(ctx context.Context) (catPawSchema, error) {
	rows, err := c.queryer.Query(ctx, c.dbPath, `
SELECT name
FROM sqlite_schema
WHERE type = 'table'
  AND name IN ('t_conversation', 'history_preview_record_ide', 'history_detail_record_ide')
ORDER BY name;`)
	if err != nil {
		return catPawSchema{}, err
	}
	schema := catPawSchema{tables: map[string]map[string]bool{}}
	for _, row := range rows {
		name := stringAny(row["name"])
		switch name {
		case "t_conversation", "history_preview_record_ide", "history_detail_record_ide":
		default:
			continue
		}
		columns, err := c.queryer.Query(ctx, c.dbPath, "PRAGMA table_info("+name+");")
		if err != nil {
			return catPawSchema{}, err
		}
		schema.tables[name] = map[string]bool{}
		for _, column := range columns {
			if field := stringAny(column["name"]); field != "" {
				schema.tables[name][field] = true
			}
		}
	}
	if !schema.supportsCurrent() && !schema.supportsLegacy() {
		return catPawSchema{}, errors.New("CatPaw history schema is unsupported")
	}
	return schema, nil
}

func (c *CatPaw) ListNativeSessions() []map[string]any {
	rows, _ := c.ListNativeSessionsWithError()
	return rows
}

// ListNativeSessionsWithError lets the API preserve its last good snapshot
// while surfacing immutable-read/schema failures in refresh_error.
func (c *CatPaw) ListNativeSessionsWithError() ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catPawQueryTimeout)
	defer cancel()
	rows, err := catPawWithImmutableSnapshot(c.dbPath, func() ([]map[string]any, error) {
		schema, err := c.discoverSchema(ctx)
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		if schema.supportsCurrent() {
			current, err := c.currentSessions(ctx, schema)
			if err != nil {
				return nil, err
			}
			rows = append(rows, current...)
		}
		if schema.supportsLegacy() {
			legacy, err := c.legacySessions(ctx, schema)
			if err != nil {
				return nil, err
			}
			rows = append(rows, legacy...)
		}
		return catPawDedupeSessions(rows, catPawSessionLimit), nil
	})
	if err != nil {
		c.setHistoryError(err)
		return nil, err
	}
	c.setHistoryError(nil)
	return rows, nil
}

func (c *CatPaw) currentSessions(ctx context.Context, schema catPawSchema) ([]map[string]any, error) {
	query := fmt.Sprintf(`
SELECT %s, %s, %s, %s, %s, %s
FROM history_preview_record_ide
WHERE conversationId IS NOT NULL AND conversationId <> ''
ORDER BY %s DESC, %s DESC
LIMIT %d;`,
		catPawSelect(schema, "history_preview_record_ide", "conversationId", "''", "native_session_id"),
		catPawSelect(schema, "history_preview_record_ide", "historyTitle", "''", "title"),
		catPawSelect(schema, "history_preview_record_ide", "projectPath", "''", "cwd"),
		catPawSelect(schema, "history_preview_record_ide", "ts", "0", "updated_ms"),
		catPawSelect(schema, "history_preview_record_ide", "mode", "''", "mode"),
		catPawSelect(schema, "history_preview_record_ide", "starred", "0", "starred"),
		catPawOrderColumn(schema, "history_preview_record_ide", "ts", "id"),
		catPawOrderColumn(schema, "history_preview_record_ide", "id", "conversationId"),
		catPawSessionLimit*2,
	)
	rows, err := c.queryer.Query(ctx, c.dbPath, query)
	if err != nil {
		return nil, err
	}
	return c.normalizeSessionRows(rows, "catpaw_sqlite_current"), nil
}

func (c *CatPaw) legacySessions(ctx context.Context, schema catPawSchema) ([]map[string]any, error) {
	query := fmt.Sprintf(`
SELECT %s, %s, %s, %s, %s, %s, %s
FROM t_conversation
WHERE conversation_id IS NOT NULL AND conversation_id <> ''
ORDER BY %s DESC, %s DESC
LIMIT %d;`,
		catPawSelect(schema, "t_conversation", "conversation_id", "''", "native_session_id"),
		catPawSelect(schema, "t_conversation", "history_title", "''", "title"),
		catPawSelect(schema, "t_conversation", "project_path", "''", "cwd"),
		catPawSelect(schema, "t_conversation", "ts", "0", "updated_ms"),
		catPawSelect(schema, "t_conversation", "created_at", "0", "created_ms"),
		catPawSelect(schema, "t_conversation", "mode", "''", "mode"),
		catPawSelect(schema, "t_conversation", "starred", "0", "starred"),
		catPawOrderColumn(schema, "t_conversation", "ts", "id"),
		catPawOrderColumn(schema, "t_conversation", "id", "conversation_id"),
		catPawSessionLimit*2,
	)
	rows, err := c.queryer.Query(ctx, c.dbPath, query)
	if err != nil {
		return nil, err
	}
	return c.normalizeSessionRows(rows, "catpaw_sqlite_legacy"), nil
}

func catPawSelect(schema catPawSchema, table, column, fallback, alias string) string {
	if schema.hasColumn(table, column) {
		return column + " AS " + alias
	}
	return fallback + " AS " + alias
}

func catPawOrderColumn(schema catPawSchema, table, preferred, fallback string) string {
	if schema.hasColumn(table, preferred) {
		return preferred
	}
	return fallback
}

func (c *CatPaw) normalizeSessionRows(rows []map[string]any, source string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, raw := range rows {
		id := strings.TrimSpace(stringAny(raw["native_session_id"]))
		if id == "" {
			continue
		}
		title := strings.TrimSpace(stringAny(raw["title"]))
		if title == "" {
			title = "CatPaw session " + shortText(id, 8)
		}
		updatedAt := msToISO(raw["updated_ms"])
		createdAt := msToISO(raw["created_ms"])
		row := map[string]any{
			"session_id":        id,
			"native_session_id": id,
			"transcript_id":     id,
			"provider_id":       c.id,
			"title":             title,
			"cwd":               nullableString(raw["cwd"]),
			"mode":              nullableString(raw["mode"]),
			"starred":           catPawBool(raw["starred"]),
			"created_at":        createdAt,
			"updated_at":        updatedAt,
			"last_reply_at":     updatedAt,
			"source":            source,
			"state":             "idle",
			"status":            "idle",
			"running":           false,
			"archived":          false,
		}
		out = append(out, row)
	}
	return out
}

func catPawDedupeSessions(rows []map[string]any, limit int) []map[string]any {
	sortByUpdated(rows)
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		id := stringAny(row["native_session_id"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, row)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func catPawBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case json.Number:
		return v.String() == "1"
	case float64:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	case string:
		return v == "1" || strings.EqualFold(v, "true")
	default:
		return false
	}
}

func (c *CatPaw) SessionMessages(sessionID string) ([]map[string]any, error) {
	if err := validateCatPawSessionID(sessionID); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), catPawQueryTimeout)
	defer cancel()
	messages, err := catPawWithImmutableSnapshot(c.dbPath, func() ([]map[string]any, error) {
		schema, err := c.discoverSchema(ctx)
		if err != nil {
			return nil, err
		}
		literal := catPawSQLString(sessionID)
		queries := []string{}
		if schema.supportsCurrent() {
			queries = append(queries, fmt.Sprintf(`
SELECT agentMessages AS messages
FROM history_detail_record_ide
WHERE conversationId = %s
ORDER BY %s DESC
LIMIT 1;`, literal, catPawOrderColumn(schema, "history_detail_record_ide", "id", "conversationId")))
		}
		if schema.supportsLegacy() {
			queries = append(queries, fmt.Sprintf(`
SELECT messages
FROM t_conversation
WHERE conversation_id = %s
ORDER BY %s DESC
LIMIT 1;`, literal, catPawOrderColumn(schema, "t_conversation", "ts", "id")))
		}
		for _, query := range queries {
			rows, err := c.queryer.Query(ctx, c.dbPath, query)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				continue
			}
			raw := strings.TrimSpace(stringAny(rows[0]["messages"]))
			if raw == "" {
				continue
			}
			if len(raw) > catPawMaxMessageJSONSize {
				return nil, errors.New("CatPaw session transcript exceeds the read-only preview limit")
			}
			messages, err := parseCatPawMessages(raw, catPawMessageLimit)
			if err != nil {
				return nil, err
			}
			return messages, nil
		}
		return []map[string]any{}, nil
	})
	if err != nil {
		c.setHistoryError(err)
		return nil, err
	}
	c.setHistoryError(nil)
	return messages, nil
}

func validateCatPawSessionID(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("CatPaw session_id is required")
	}
	if len(sessionID) > 256 || strings.ContainsRune(sessionID, '\x00') {
		return errors.New("CatPaw session_id is invalid")
	}
	return nil
}

func catPawSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func parseCatPawMessages(raw string, limit int) ([]map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var records []any
	if err := decoder.Decode(&records); err != nil {
		return nil, errors.New("CatPaw session transcript is invalid JSON")
	}
	out := make([]map[string]any, 0, len(records))
	for _, value := range records {
		record := mapAny(value)
		if len(record) == 0 {
			continue
		}
		text := catPawMessageText(record["content"])
		role := catPawRole(stringAny(record["role"]))
		if text == "" && role == "" {
			continue
		}
		if role == "" {
			role = "unknown"
		}
		item := map[string]any{
			"role": role,
			"kind": "text",
			"text": text,
		}
		if id := firstNonEmpty(stringAny(record["messageId"]), stringAny(record["message_id"])); id != "" {
			item["id"] = id
		}
		if ts := catPawMessageTimestamp(record); ts != "" {
			item["ts"] = ts
		}
		if status := strings.TrimSpace(stringAny(record["streamStatus"])); status != "" {
			item["stream_status"] = status
		}
		out = append(out, item)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func catPawMessageText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts := []string{}
	for _, value := range listAny(content) {
		if text, ok := value.(string); ok && text != "" {
			parts = append(parts, text)
			continue
		}
		block := mapAny(value)
		if stringAny(block["type"]) == "text" {
			if text := stringAny(block["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func catPawRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "human":
		return "user"
	case "assistant", "ai", "model":
		return "assistant"
	case "system":
		return "system"
	case "tool":
		return "tool"
	default:
		return ""
	}
}

func catPawMessageTimestamp(record map[string]any) string {
	for _, key := range []string{"timestamp", "createdAt", "created_at", "ts"} {
		value := record[key]
		if text, ok := value.(string); ok && text != "" {
			return text
		}
		if stamp := msToISO(value); stamp != "" {
			return stamp
		}
	}
	return ""
}

func (c *CatPaw) SessionModel(string) map[string]any { return map[string]any{} }

func (c *CatPaw) ReferencedFiles(string) map[string]bool { return map[string]bool{} }

func (c *CatPaw) OpenOrCreateSession(string, StartOptions) (string, error) {
	return "", errCatPawReadOnly
}

func (c *CatPaw) CloseSession(string) map[string]any { return catPawReadOnlyFailure() }

func (c *CatPaw) SendPrompt(string, string) SendResult {
	message := errCatPawReadOnly.Error()
	return SendResult{OK: false, State: "error", Message: message, Error: &message}
}

func (c *CatPaw) LatestOutput(sessionID string) map[string]any {
	messages, err := c.SessionMessages(sessionID)
	if err != nil {
		return map[string]any{
			"source": "catpaw_sqlite", "text": "", "approval_required": false,
			"error": err.Error(),
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if stringAny(messages[i]["role"]) == "assistant" {
			return map[string]any{
				"source": "catpaw_sqlite", "text": stringAny(messages[i]["text"]),
				"approval_required": false,
			}
		}
	}
	return map[string]any{"source": "catpaw_sqlite", "text": "", "approval_required": false}
}

func (c *CatPaw) DetectState(string) string { return "idle" }

func (c *CatPaw) RelayApproval(string, string) map[string]any { return catPawReadOnlyFailure() }

func (c *CatPaw) SendKeys(string, []string) map[string]any { return catPawReadOnlyFailure() }

func (c *CatPaw) Interrupt(string) map[string]any { return catPawReadOnlyFailure() }

func (c *CatPaw) SetSessionModel(string, string, string) map[string]any {
	return catPawReadOnlyFailure()
}

func catPawReadOnlyFailure() map[string]any {
	return map[string]any{"ok": false, "detail": errCatPawReadOnly.Error()}
}

func (c *CatPaw) setHistoryError(err error) {
	c.mu.Lock()
	c.lastHistoryErr = err
	c.lastHistoryRead = time.Now()
	c.mu.Unlock()
}

// Compile-time contract checks keep a future write-capable interface from
// silently replacing these explicit read-only stubs.
var _ Provider = (*CatPaw)(nil)
var _ ProviderProfiler = (*CatPaw)(nil)
