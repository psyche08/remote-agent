package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

func TestSessionListEndpointsHideSubagentsButExactLookupsStillWork(t *testing.T) {
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.SaveSessions([]state.Record{
		{
			"session_id": "logical-child", "provider_id": "codex", "transcript_id": "thread-child",
			"title": "child", hiddenFromSessionListsKey: true,
		},
		{
			"session_id": "logical-normal", "provider_id": "codex", "transcript_id": "thread-normal",
			"title": "normal",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTasks([]state.Record{
		{"task_id": "task-child", "session_id": "logical-child", "provider_id": "codex", "status": "completed"},
		{"task_id": "task-normal", "session_id": "logical-normal", "provider_id": "codex", "status": "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{id: "codex"}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	assertVisibleIDs(t, srv, "/sessions", "sessions", "session_id", []string{"logical-normal"})
	assertVisibleIDs(t, srv, "/sessions?provider_id=codex&session_id=logical-child", "sessions", "session_id", []string{"logical-child"})
	assertVisibleIDs(t, srv, "/tasks", "tasks", "task_id", []string{"task-normal"})
	assertVisibleIDs(t, srv, "/tasks?session_id=logical-child", "tasks", "task_id", []string{"task-child"})

	if rec, ok, err := srv.findSessionForProviderAny("codex", "thread-child"); err != nil || !ok || recordString(rec, "session_id") != "logical-child" {
		t.Fatalf("hidden session was not directly resolvable: rec=%#v ok=%v err=%v", rec, ok, err)
	}
}

func TestNativeAndLiveListsHideSubagentRowsWithoutChangingInternalLookup(t *testing.T) {
	st := state.New(filepath.Join(t.TempDir(), "data"))
	hidden := map[string]any{
		"session_id": "thread-child", "cli_session_id": "thread-child", "native_session_id": "thread-child",
		"transcript_id": "thread-child", "title": "child", "provider_id": "codex",
		"live": true, hiddenFromSessionListsKey: true,
	}
	normal := map[string]any{
		"session_id": "thread-normal", "cli_session_id": "thread-normal", "native_session_id": "thread-normal",
		"transcript_id": "thread-normal", "title": "normal", "provider_id": "codex", "live": true,
	}
	fp := &fakePushProvider{
		id: "codex",
		native: []map[string]any{
			cloneVisibilityTestRow(hidden),
			cloneVisibilityTestRow(normal),
		},
		live: []map[string]any{
			cloneVisibilityTestRow(hidden),
			cloneVisibilityTestRow(normal),
		},
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	assertVisibleIDs(t, srv, "/native_sessions?provider_id=codex&sync=1", "sessions", "native_session_id", []string{"thread-normal"})
	assertVisibleIDs(t, srv, "/live_sessions?provider_id=codex", "sessions", "transcript_id", []string{"thread-normal"})

	if row, ok := srv.nativeSessionByID("codex", fp, "thread-child"); !ok || stringAny(row["native_session_id"]) != "thread-child" {
		t.Fatalf("hidden native row was not directly resolvable: %#v ok=%v", row, ok)
	}
}

func TestDirectCodexSubagentSessionIsPersistedHidden(t *testing.T) {
	st := state.New(filepath.Join(t.TempDir(), "data"))
	fp := &directCodexProvider{
		fakePushProvider: fakePushProvider{
			id: "codex",
			native: []map[string]any{{
				"cli_session_id": "thread-child", "native_session_id": "thread-child",
				"title": "child", hiddenFromSessionListsKey: true,
			}},
		},
		sentSession: make(chan string, 1),
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rec, err := srv.prepareDirectCodexSession(fp, "codex", "thread-child")
	if err != nil || rec == nil || !truthy(rec[hiddenFromSessionListsKey], false) {
		t.Fatalf("direct child record=%#v err=%v", rec, err)
	}
	assertVisibleIDs(t, srv, "/sessions", "sessions", "session_id", nil)
	assertVisibleIDs(t, srv, "/sessions?provider_id=codex&session_id="+recordString(rec, "session_id"), "sessions", "session_id", []string{recordString(rec, "session_id")})
}

func TestTaskVisibilityRemainsProviderScoped(t *testing.T) {
	hidden := map[string]bool{
		"codex\x00shared-session": true,
		"\x00shared-session":      true,
	}
	if taskHiddenFromLists(state.Record{
		"provider_id": "claude", "session_id": "shared-session",
	}, hidden) {
		t.Fatal("a hidden Codex session hid an unrelated Claude task with the same ID")
	}
	if !taskHiddenFromLists(state.Record{
		"session_id": "shared-session",
	}, hidden) {
		t.Fatal("an unscoped legacy task did not inherit its hidden session")
	}
}

func assertVisibleIDs(t *testing.T, srv *Server, target string, collection string, idKey string, want []string) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", target, rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	raw, _ := body[collection].([]any)
	got := make([]string, 0, len(raw))
	for _, item := range raw {
		got = append(got, stringAny(item.(map[string]any)[idKey]))
	}
	if len(got) != len(want) {
		t.Fatalf("%s IDs=%#v want=%#v body=%s", target, got, want, rr.Body.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s IDs=%#v want=%#v", target, got, want)
		}
	}
}

func cloneVisibilityTestRow(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for key, value := range row {
		out[key] = value
	}
	return out
}
