package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
)

type fakeCatPawQueryer struct {
	tables                          map[string][]string
	currentRows, legacyRows         []map[string]any
	currentMessages, legacyMessages string
	queries                         []string
	afterQuery                      func(int, string)
}

func (f *fakeCatPawQueryer) Query(_ context.Context, _ string, query string) ([]map[string]any, error) {
	f.queries = append(f.queries, query)
	if f.afterQuery != nil {
		defer f.afterQuery(len(f.queries), query)
	}
	switch {
	case strings.Contains(query, "FROM sqlite_schema"):
		rows := []map[string]any{}
		for _, name := range []string{"history_detail_record_ide", "history_preview_record_ide", "t_conversation"} {
			if f.tables[name] != nil {
				rows = append(rows, map[string]any{"name": name})
			}
		}
		return rows, nil
	case strings.Contains(query, "PRAGMA table_info("):
		for name, columns := range f.tables {
			if strings.Contains(query, "table_info("+name+")") {
				rows := make([]map[string]any, len(columns))
				for i, column := range columns {
					rows[i] = map[string]any{"name": column}
				}
				return rows, nil
			}
		}
	case strings.Contains(query, "SELECT agentMessages AS messages"):
		if f.currentMessages == "" {
			return []map[string]any{}, nil
		}
		return []map[string]any{{"messages": f.currentMessages}}, nil
	case strings.Contains(query, "SELECT messages"):
		if f.legacyMessages == "" {
			return []map[string]any{}, nil
		}
		return []map[string]any{{"messages": f.legacyMessages}}, nil
	case strings.Contains(query, "FROM history_preview_record_ide"):
		return f.currentRows, nil
	case strings.Contains(query, "FROM t_conversation"):
		return f.legacyRows, nil
	}
	return nil, errors.New("unexpected CatPaw test query")
}

func newFakeCatPaw(t *testing.T, queryer *fakeCatPawQueryer) *CatPaw {
	t.Helper()
	db := filepath.Join(t.TempDir(), "globalCache.sqlite")
	if err := os.WriteFile(db, []byte("synthetic CatPaw database generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewCatPaw("catpaw", config.ProviderConfig{Extra: map[string]any{
		"history_db_path": db,
	}})
	p.queryer = queryer
	return p
}

func catPawFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "catpaw", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func catPawCurrentColumns() map[string][]string {
	return map[string][]string{
		"history_preview_record_ide": {"id", "conversationId", "historyTitle", "ts", "chatApplyModeType", "mode", "starred", "projectPath", "ideType", "env", "remoteRepoUrl"},
		"history_detail_record_ide":  {"id", "conversationId", "agentMessages", "checkpoints"},
	}
}

func catPawLegacyColumns() map[string][]string {
	return map[string][]string{
		"t_conversation": {"id", "conversation_id", "history_title", "ts", "chat_apply_mode_type", "mode", "starred", "project_path", "ide_type", "env", "remote_repo_url", "messages", "checkpoints", "created_at", "todos", "fileMap", "plan_status", "mis_id"},
	}
}

func TestCatPawCurrentSchemaDiscoveryAndMessages(t *testing.T) {
	fake := &fakeCatPawQueryer{
		tables: catPawCurrentColumns(), currentMessages: catPawFixture(t, "current_messages.json"),
		currentRows: []map[string]any{{"native_session_id": "current-session", "title": "Synthetic current", "cwd": "/synthetic/current", "updated_ms": json.Number("1767225600000"), "mode": "agent", "starred": json.Number("1")}},
	}
	p := newFakeCatPaw(t, fake)
	rows := p.ListNativeSessions()
	if len(rows) != 1 || rows[0]["native_session_id"] != "current-session" || rows[0]["source"] != "catpaw_sqlite_current" || rows[0]["starred"] != true {
		t.Fatalf("rows=%#v", rows)
	}
	messages, err := p.SessionMessages("current-session")
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	if messages[0]["role"] != "user" || messages[0]["text"] != "synthetic current question" || messages[0]["ts"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("first=%#v", messages[0])
	}
	if messages[1]["role"] != "assistant" || messages[1]["text"] != "synthetic current answer" {
		t.Fatalf("second=%#v", messages[1])
	}
}

func TestCatPawLegacySchemaAndMigrationDedupe(t *testing.T) {
	tables := catPawLegacyColumns()
	for name, cols := range catPawCurrentColumns() {
		tables[name] = cols
	}
	fake := &fakeCatPawQueryer{
		tables: tables, currentMessages: catPawFixture(t, "current_messages.json"), legacyMessages: catPawFixture(t, "legacy_messages.json"),
		currentRows: []map[string]any{{"native_session_id": "shared-session", "title": "Current", "updated_ms": json.Number("1767225600000")}},
		legacyRows:  []map[string]any{{"native_session_id": "shared-session", "title": "Legacy", "updated_ms": json.Number("1735689600000"), "created_ms": json.Number("1735689500000")}},
	}
	p := newFakeCatPaw(t, fake)
	rows := p.ListNativeSessions()
	if len(rows) != 1 || rows[0]["title"] != "Current" {
		t.Fatalf("rows=%#v", rows)
	}
	messages, err := p.SessionMessages("shared-session")
	if err != nil || len(messages) != 2 || messages[1]["text"] != "synthetic current answer" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestCatPawEmptyCurrentDetailFallsBackToLegacyMessages(t *testing.T) {
	tables := catPawLegacyColumns()
	for name, cols := range catPawCurrentColumns() {
		tables[name] = cols
	}
	fake := &fakeCatPawQueryer{
		tables: tables, currentMessages: "   ",
		legacyMessages: catPawFixture(t, "legacy_messages.json"),
	}
	p := newFakeCatPaw(t, fake)
	messages, err := p.SessionMessages("mixed-session")
	if err != nil || len(messages) != 2 || messages[1]["text"] != "synthetic legacy answer" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestCatPawUnknownSchemaFailsClosed(t *testing.T) {
	fake := &fakeCatPawQueryer{
		tables: map[string][]string{
			"history_preview_record_ide": {"id", "conversationId"},
			"history_detail_record_ide":  {"id", "conversationId", "renamedMessages"},
		},
		currentRows: []map[string]any{{"native_session_id": "must-not-leak"}},
	}
	p := newFakeCatPaw(t, fake)
	rows, err := p.ListNativeSessionsWithError()
	if err == nil || !strings.Contains(err.Error(), "schema is unsupported") {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	if len(rows) != 0 {
		t.Fatalf("unsupported schema returned rows: %#v", rows)
	}
}

func TestCatPawIncompleteCurrentSchemaUsesSupportedLegacyOnly(t *testing.T) {
	tables := catPawLegacyColumns()
	tables["history_preview_record_ide"] = []string{"id", "conversationId"}
	tables["history_detail_record_ide"] = []string{"id", "conversationId", "renamedMessages"}
	fake := &fakeCatPawQueryer{
		tables:      tables,
		currentRows: []map[string]any{{"native_session_id": "unsupported-current"}},
		legacyRows:  []map[string]any{{"native_session_id": "legacy-session", "title": "Legacy"}},
	}
	p := newFakeCatPaw(t, fake)
	rows, err := p.ListNativeSessionsWithError()
	if err != nil || len(rows) != 1 || rows[0]["native_session_id"] != "legacy-session" {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	for _, query := range fake.queries {
		if strings.Contains(query, "FROM history_preview_record_ide") {
			t.Fatalf("queried unsupported current schema: %s", query)
		}
	}
}

func TestCatPawSignatureFailureIsVisibleAndActionsFailClosed(t *testing.T) {
	app := filepath.Join(t.TempDir(), "CatPaw.app")
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "globalCache.sqlite")
	if err := os.WriteFile(db, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewCatPaw("catpaw", config.ProviderConfig{Extra: map[string]any{
		"app_path": app, "history_db_path": db,
	}})
	p.sigProbe = func(context.Context, string) error { return errors.New("invalid") }
	status := p.Status()
	if !status.Installed || status.State != "error" || status.LastError == nil || !strings.Contains(*status.LastError, "signature") || status.Capabilities["signature_valid"] || !status.Capabilities["history_db_private"] || status.Capabilities["computer_use"] || status.Capabilities["locked_use"] {
		t.Fatalf("status=%#v", status)
	}
	for _, action := range []ActionID{ActionSendPrompt, ActionCreate, ActionResume, ActionClose, ActionInterrupt, ActionSteer, ActionApproval, ActionQuestion, ActionRawKeys, ActionSetModel, ActionUpload, ActionRewind} {
		if p.SupportsAction(action) {
			t.Fatalf("action %s unexpectedly supported", action)
		}
	}
	for _, action := range Actions(p) {
		if action.Supported {
			t.Fatalf("typed action %s unexpectedly supported", action.ID)
		}
	}
	if _, err := p.OpenOrCreateSession("s", StartOptions{}); !errors.Is(err, errCatPawReadOnly) {
		t.Fatalf("open err=%v", err)
	}
	if p.SendPrompt("s", "x").OK || p.CloseSession("s")["ok"] != false || p.RelayApproval("s", "allow")["ok"] != false || p.SendKeys("s", []string{"ENTER"})["ok"] != false || p.Interrupt("s")["ok"] != false || p.SetSessionModel("s", "m", "e")["ok"] != false {
		t.Fatal("a CatPaw mutation did not fail closed")
	}
	profile := p.ProviderProfile()
	if profile.Family != ProviderFamilyCatPaw || profile.AdapterKind != AdapterKindDesktopTranscript || profile.RuntimeNamespace != RuntimeNamespaceCatPaw {
		t.Fatalf("profile=%#v", profile)
	}
}

func TestCatPawSignatureIdentityRequiresExactBundleAndTeam(t *testing.T) {
	valid := []byte("Executable=/synthetic/CatPaw\nIdentifier=com.meituan.catpaw\nTeamIdentifier=BHWTW6L8X6\n")
	if err := validateCatPawSignatureIdentity(valid); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	for name, output := range map[string][]byte{
		"wrong bundle": []byte("Identifier=com.meituan.catpaw.helper\nTeamIdentifier=BHWTW6L8X6\n"),
		"wrong team":   []byte("Identifier=com.meituan.catpaw\nTeamIdentifier=AAAAAAAAAA\n"),
		"missing team": []byte("Identifier=com.meituan.catpaw\n"),
		"prefix only":  []byte("Identifier=com.meituan.catpaw.extra\nTeamIdentifier=BHWTW6L8X6\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatPawSignatureIdentity(output); err == nil {
				t.Fatal("mismatched identity was accepted")
			}
		})
	}
}

func TestCatPawImmutableReadRejectsActiveWAL(t *testing.T) {
	db := filepath.Join(t.TempDir(), "globalCache.sqlite")
	if err := os.WriteFile(db, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-wal", []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catPawImmutablePreflight(db); !errors.Is(err, errCatPawActiveWAL) {
		t.Fatalf("err=%v", err)
	}
}

func TestCatPawImmutableReadRejectsRollbackJournalAndSymlink(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "globalCache.sqlite")
	if err := os.WriteFile(db, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-journal", []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := catPawImmutablePreflight(db); !errors.Is(err, errCatPawActiveJournal) {
		t.Fatalf("journal err=%v", err)
	}
	if err := os.Remove(db + "-journal"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.sqlite")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	if _, err := catPawImmutablePreflight(link); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink err=%v", err)
	}
}

func TestCatPawImmutablePostflightRejectsSameSizeReplacement(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "globalCache.sqlite")
	if err := os.WriteFile(db, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := catPawImmutablePreflight(db)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.sqlite")
	if err := os.Rename(db, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(db, before.modTime, before.modTime); err != nil {
		t.Fatal(err)
	}
	if err := catPawImmutablePostflight(db, before); !errors.Is(err, errCatPawDatabaseChanged) {
		t.Fatalf("replacement err=%v", err)
	}
}

func TestCatPawLogicalReadsRejectDatabaseChangeBetweenQueries(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(*CatPaw) error
	}{
		{
			name: "session list",
			read: func(p *CatPaw) error {
				rows, err := p.ListNativeSessionsWithError()
				if len(rows) != 0 {
					t.Fatalf("changed snapshot returned rows: %#v", rows)
				}
				return err
			},
		},
		{
			name: "session messages",
			read: func(p *CatPaw) error {
				messages, err := p.SessionMessages("synthetic-session")
				if len(messages) != 0 {
					t.Fatalf("changed snapshot returned messages: %#v", messages)
				}
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCatPawQueryer{
				tables:         catPawLegacyColumns(),
				legacyRows:     []map[string]any{{"native_session_id": "synthetic-session"}},
				legacyMessages: catPawFixture(t, "legacy_messages.json"),
			}
			p := newFakeCatPaw(t, fake)
			fake.afterQuery = func(number int, _ string) {
				if number != 1 {
					return
				}
				if err := os.WriteFile(
					p.dbPath, []byte("synthetic changed CatPaw database generation"), 0o600,
				); err != nil {
					t.Fatal(err)
				}
			}
			err := tc.read(p)
			if !errors.Is(err, errCatPawDatabaseChanged) {
				t.Fatalf("logical read err=%v, want database-changed refusal", err)
			}
		})
	}
}

func TestCatPawSessionIDIsSQLQuoted(t *testing.T) {
	got := catPawSQLString("id'; DROP TABLE t_conversation; --")
	if got != "'id''; DROP TABLE t_conversation; --'" {
		t.Fatalf("quoted=%q", got)
	}
}

func TestCatPawLocalReadOnlyIntegration(t *testing.T) {
	if os.Getenv("AGENTHALO_CATPAW_INTEGRATION") != "1" {
		t.Skip("set AGENTHALO_CATPAW_INTEGRATION=1 to read the local CatPaw history")
	}
	p := NewCatPaw("catpaw", config.ProviderConfig{})
	rows := p.ListNativeSessions()
	if len(rows) == 0 {
		p.mu.Lock()
		err := p.lastHistoryErr
		p.mu.Unlock()
		t.Fatalf("no CatPaw sessions discovered: %v", err)
	}
	id := stringAny(rows[0]["native_session_id"])
	if id == "" {
		t.Fatal("CatPaw session is missing its native id")
	}
	messages, err := p.SessionMessages(id)
	if err != nil || len(messages) == 0 {
		t.Fatalf("CatPaw transcript preview failed: count=%d err=%v", len(messages), err)
	}
	for index, message := range messages {
		role := stringAny(message["role"])
		if role != "user" && role != "assistant" && role != "system" && role != "tool" && role != "unknown" {
			t.Fatalf("message %d has unsafe role %q", index, role)
		}
	}
}
