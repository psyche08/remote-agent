package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

type blockingNativeProvider struct {
	fakePushProvider
	started chan struct{}
	release chan struct{}
	rows    []map[string]any
}

func (f *blockingNativeProvider) ID() string { return "codex" }

func (f *blockingNativeProvider) ListNativeSessions() []map[string]any {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-f.release
	return f.rows
}

type nativeListResult struct {
	rows     []map[string]any
	err      error
	panicNow bool
}

type sessionListResponse struct {
	Sessions   []map[string]any `json:"sessions"`
	DeviceTime map[string]any   `json:"device_time"`
}

type scriptedNativeProvider struct {
	fakePushProvider
	mu      sync.Mutex
	results []nativeListResult
	calls   int
}

func (f *scriptedNativeProvider) ID() string { return "codex" }

func (f *scriptedNativeProvider) ListNativeSessions() []map[string]any {
	rows, _ := f.ListNativeSessionsWithError()
	return rows
}

func (f *scriptedNativeProvider) ListNativeSessionsWithError() ([]map[string]any, error) {
	f.mu.Lock()
	index := f.calls
	f.calls++
	if index >= len(f.results) {
		index = len(f.results) - 1
	}
	result := f.results[index]
	f.mu.Unlock()
	if result.panicNow {
		panic("native discovery test panic")
	}
	return result.rows, result.err
}

type refreshGateNativeProvider struct {
	fakePushProvider
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	initial []map[string]any
	fresh   []map[string]any
}

func (f *refreshGateNativeProvider) ID() string { return "codex" }

func (f *refreshGateNativeProvider) ListNativeSessions() []map[string]any {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call == 1 {
		return f.initial
	}
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-f.release
	return f.fresh
}

func (f *refreshGateNativeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type blockingSendProvider struct {
	fakePushProvider
	started chan struct{}
	release chan struct{}
	result  provider.SendResult
}

type directCodexProvider struct {
	fakePushProvider
	boundSession    string
	boundTranscript string
	boundRoute      string
	boundCwd        string
	desktopBound    bool
	nativeRoute     string
	appServerRoute  string
	sentSession     chan string
}

func (f *directCodexProvider) ID() string { return "codex" }
func (f *directCodexProvider) NativeDeliveryRoute() string {
	return firstNonEmpty(f.nativeRoute, "shared_daemon")
}
func (f *directCodexProvider) AppServerDeliveryRoute() string {
	return firstNonEmpty(f.appServerRoute, "shared_daemon")
}
func (f *directCodexProvider) BindTranscript(sessionID string, transcriptID string) {
	f.boundSession, f.boundTranscript = sessionID, transcriptID
}
func (f *directCodexProvider) BindDesktopTranscript(sessionID string, transcriptID string) {
	f.desktopBound = true
	f.BindTranscript(sessionID, transcriptID)
}
func (f *directCodexProvider) BindSessionRoute(sessionID string, transcriptID string, route string, cwd string) {
	f.boundSession, f.boundTranscript = sessionID, transcriptID
	f.boundRoute, f.boundCwd = route, cwd
	f.desktopBound = route == "desktop_ipc"
}
func (f *directCodexProvider) SendPrompt(sessionID string, _ string) provider.SendResult {
	f.sentSession <- sessionID
	return provider.SendResult{OK: true, State: "running", NativeTaskID: "turn-1"}
}

type createCodexProvider struct {
	directCodexProvider
	nativeID string
}

func (f *createCodexProvider) OpenOrCreateSession(string, provider.StartOptions) (string, error) {
	return f.nativeID, nil
}

type resumeCodexProvider struct {
	directCodexProvider
	resumeSession string
	resumeID      string
	resumeCwd     string
	resumeFork    bool
}

func (f *resumeCodexProvider) OpenResumeSession(sessionID string, resumeID string, cwd string, fork bool) (string, error) {
	f.resumeSession, f.resumeID, f.resumeCwd, f.resumeFork = sessionID, resumeID, cwd, fork
	return resumeID, nil
}

type desktopOnlyCodexProvider struct {
	fakePushProvider
	boundSession    string
	boundTranscript string
	desktopBound    bool
}

func (f *desktopOnlyCodexProvider) ID() string { return "codex" }
func (f *desktopOnlyCodexProvider) BindTranscript(sessionID string, transcriptID string) {
	f.boundSession, f.boundTranscript = sessionID, transcriptID
}
func (f *desktopOnlyCodexProvider) BindDesktopTranscript(sessionID string, transcriptID string) {
	f.desktopBound = true
	f.BindTranscript(sessionID, transcriptID)
}

type attachmentSendProvider struct {
	fakePushProvider
	got chan []provider.Attachment
}

type usagePreviewProvider struct{ fakePushProvider }

func (f *usagePreviewProvider) SessionMessages(string) ([]map[string]any, error) {
	return []map[string]any{{
		"role": "assistant", "kind": "turn_usage", "usage": map[string]any{
			"model": "claude-opus-4-8", "input_tokens": int64(1000), "output_tokens": int64(200),
			"cache_creation_input_tokens": int64(300), "cache_read_input_tokens": int64(4000),
			"duration_ms": int64(2500), "models": []map[string]any{{
				"model": "claude-opus-4-8", "input_tokens": int64(1000), "output_tokens": int64(200),
				"cache_creation_input_tokens": int64(300), "cache_read_input_tokens": int64(4000), "total_tokens": int64(5500),
			}},
		},
	}}, nil
}

func (f *usagePreviewProvider) SessionModel(string) map[string]any {
	return map[string]any{"model": "claude-opus-4-8", "usage": []map[string]any{{
		"model": "claude-opus-4-8", "input_tokens": int64(1000), "output_tokens": int64(200),
		"cache_creation_input_tokens": int64(300), "cache_read_input_tokens": int64(4000), "total_tokens": int64(5500),
	}}}
}

func TestSessionPreviewAddsAPIEstimatedCosts(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"claude": &usagePreviewProvider{}}, state.New(filepath.Join(t.TempDir(), "data")))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/session_preview?provider_id=claude&session_id=s1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows := body["usage"].([]any)
	if rows[0].(map[string]any)["cost_usd"] == nil {
		t.Fatalf("footer cost missing: %#v", body["usage"])
	}
	messages := body["messages"].([]any)
	turnUsage := messages[0].(map[string]any)["usage"].(map[string]any)
	if turnUsage["cost_usd"] == nil || turnUsage["cost_known"] != true {
		t.Fatalf("turn cost missing: %#v", turnUsage)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/pricing", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "platform.claude.com") {
		t.Fatalf("pricing status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func (f *attachmentSendProvider) ID() string { return "claude" }
func (f *attachmentSendProvider) SendPromptWithAttachments(_ string, _ string, attachments []provider.Attachment) provider.SendResult {
	f.got <- attachments
	return provider.SendResult{OK: true, State: "running", NativeTaskID: "native-attachment"}
}

func TestUploadIsSessionScopedAndDeliveredAsAttachment(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{"session_id": "logical-1", "provider_id": "claude", "title": "Upload"}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	fp := &attachmentSendProvider{got: make(chan []provider.Attachment, 1)}
	srv := NewServer(cfg, provider.Registry{"claude": fp}, st)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("provider_id", "claude")
	_ = mw.WriteField("session_id", "logical-1")
	part, err := mw.CreateFormFile("file", "sample.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("not-a-real-png-but-private"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", rr.Code, rr.Body.String())
	}
	var uploaded struct {
		Attachment struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"attachment"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.Attachment.ID == "" || uploaded.Attachment.Name != "sample.png" {
		t.Fatalf("bad upload response: %#v", uploaded)
	}

	payload, _ := json.Marshal(map[string]any{"provider_id": "claude", "session_id": "logical-1", "prompt": "", "attachments": []string{uploaded.Attachment.ID}})
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/send_prompt", bytes.NewReader(payload))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case got := <-fp.got:
		if len(got) != 1 || got[0].ID != uploaded.Attachment.ID || got[0].Path == "" {
			t.Fatalf("bad attachments: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("attachment was not delivered")
	}
	deadline := time.Now().Add(time.Second)
	for srv.isSendInFlight("claude", "logical-1") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.isSendInFlight("claude", "logical-1") {
		t.Fatal("attachment send did not finish")
	}

	other, _ := json.Marshal(map[string]any{"provider_id": "claude", "session_id": "other", "prompt": "x", "attachments": []string{uploaded.Attachment.ID}})
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/send_prompt", bytes.NewReader(other))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-session status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadRejectsProviderWithoutAttachmentCapability(t *testing.T) {
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.SaveSessions([]state.Record{{"session_id": "logical-1", "provider_id": "terminal-agent"}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"terminal-agent": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"terminal-agent": &fakePushProvider{id: "terminal-agent"}}, st)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("provider_id", "terminal-agent")
	_ = mw.WriteField("session_id", "logical-1")
	part, err := mw.CreateFormFile("file", "unused.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("must not be persisted"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "does not support attachments") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(srv.uploadDir("terminal-agent", "logical-1")); !os.IsNotExist(err) {
		t.Fatalf("unsupported upload created persistent data: %v", err)
	}
}

func (f *blockingSendProvider) ID() string { return "codex" }

func (f *blockingSendProvider) Status() provider.Status {
	return provider.Status{ProviderID: "codex", IsRunning: true, State: f.state}
}

func (f *blockingSendProvider) SendPrompt(string, string) provider.SendResult {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-f.release
	if f.result.State == "" {
		f.result.State = "running"
	}
	return f.result
}

func TestStatusShape(t *testing.T) {
	cfg := &config.Config{
		DeviceID:        "device-a",
		Devices:         []string{"device-a", "device-b"},
		DefaultProvider: "claude",
		Providers: map[string]config.ProviderConfig{
			"claude": {AppName: "Claude Code CLI", Command: "claude", Cwd: "~/Developer"},
		},
	}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.BuildRegistry(cfg), state.New(filepath.Join(t.TempDir(), "data")))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["device_id"] != "device-a" || body["active_provider"] != "claude" {
		t.Fatalf("bad body: %#v", body)
	}
	if _, ok := body["model_select"].(map[string]any); !ok {
		t.Fatalf("missing model_select: %#v", body)
	}
	if _, ok := body["version"].(map[string]any); !ok {
		t.Fatalf("missing version: %#v", body)
	}
}

func TestHealthzIncludesVersion(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.BuildRegistry(cfg), state.New(filepath.Join(t.TempDir(), "data")))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["version"].(map[string]any); !ok {
		t.Fatalf("missing version: %#v", body)
	}
}

func TestClaudeProviderAliasesShareHistoricalSessions(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "logical-old", "provider_id": "claude_cli",
		"native_session_id": "019ef769-7611-70e0-839a-283dc0e5f256",
		"transcript_id":     "019ef769-7611-70e0-839a-283dc0e5f256",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "claude_cli", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"claude": &fakePushProvider{id: "claude"}}, st)
	if _, resolved, ok := srv.getProvider("claude_cli"); !ok || resolved != "claude" || srv.activeProvider != "claude" {
		t.Fatalf("Claude alias not canonicalized: resolved=%q active=%q ok=%v", resolved, srv.activeProvider, ok)
	}
	if rec, ok, err := srv.findSessionForProviderAny("claude", "logical-old"); err != nil || !ok || recordString(rec, "provider_id") != "claude_cli" {
		t.Fatalf("historical CLI record not found through canonical provider: rec=%#v ok=%v err=%v", rec, ok, err)
	}
	if rows := srv.storedNativeSessions("claude"); len(rows) != 1 || rows[0]["provider_id"] != "claude" {
		t.Fatalf("historical CLI record not merged into Claude sessions: %#v", rows)
	}
}

func TestSendPromptReturnsBeforeProviderDelivery(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "logical-1", "provider_id": "codex", "title": "Inspect",
		"native_session_id": "thread-1", "transcript_id": "thread-1",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &blockingSendProvider{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  provider.SendResult{OK: true, State: "running", NativeTaskID: "thread-1"},
	}
	released := false
	defer func() {
		if !released {
			close(fp.release)
		}
	}()
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(`{"provider_id":"codex","session_id":"logical-1","prompt":"slow desktop ipc"}`))
	done := make(chan struct{})
	start := time.Now()
	go func() {
		srv.Handler().ServeHTTP(rr, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("send_prompt blocked on provider delivery")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("send_prompt returned too slowly: %s", elapsed)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["accepted"] != true {
		t.Fatalf("bad body: %#v", body)
	}
	if body["state"] != "delivering" {
		t.Fatalf("accepted send must remain delivering until provider confirmation: %#v", body)
	}
	select {
	case <-fp.started:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("provider send was not started in the background")
	}
	statusRR := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "/status?provider_id=codex&session_id=logical-1", nil)
	srv.Handler().ServeHTTP(statusRR, statusReq)
	var statusBody map[string]any
	if err := json.Unmarshal(statusRR.Body.Bytes(), &statusBody); err != nil {
		t.Fatal(err)
	}
	if statusBody["state"] != "delivering" {
		t.Fatalf("in-flight provider call exposed as a running turn: %#v", statusBody)
	}
	close(fp.release)
	released = true
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tasks, err := st.Tasks()
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) == 1 && recordString(tasks[0], "status") == "running" && recordString(tasks[0], "native_task_id") == "thread-1" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tasks, _ := st.Tasks()
	t.Fatalf("task was not completed by background send: %#v", tasks)
}

func TestDesktopSendAttachesInBackgroundAndStreamsResult(t *testing.T) {
	ownerErr := "codex thread has no active Desktop IPC owner; reattach the thread in Codex Desktop"
	tests := []struct {
		name       string
		result     provider.SendResult
		wantStatus string
	}{
		{
			name:       "owner attaches",
			result:     provider.SendResult{OK: true, State: "running", NativeTaskID: "turn-1"},
			wantStatus: "running",
		},
		{
			name:       "owner attach times out",
			result:     provider.SendResult{OK: false, State: "error", Error: &ownerErr},
			wantStatus: "error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := state.New(filepath.Join(dir, "data"))
			if err := st.SaveSessions([]state.Record{{
				"session_id": "logical-1", "provider_id": "codex", "title": "Desktop",
				"native_session_id": "thread-1", "transcript_id": "thread-1", "delivery_route": "desktop_ipc",
			}}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
			config.ApplyDefaults(cfg)
			fp := &blockingSendProvider{
				started: make(chan struct{}, 1),
				release: make(chan struct{}),
				result:  tc.result,
			}
			released := false
			defer func() {
				if !released {
					close(fp.release)
				}
			}()
			srv := NewServer(cfg, provider.Registry{"codex": fp}, st)
			// The PWA subscribes native Codex previews by transcript id, not by
			// the logical remote-agent session id.
			stream := srv.subscribeStream("codex", "thread-1")
			defer srv.unsubscribeStream("codex", "thread-1", stream)

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(`{"provider_id":"codex","session_id":"logical-1","prompt":"wait for desktop"}`))
			start := time.Now()
			srv.Handler().ServeHTTP(rr, req)
			if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
				t.Fatalf("send_prompt waited for Desktop owner: %s", elapsed)
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusOK || body["accepted"] != true || body["state"] != "attaching" {
				t.Fatalf("status=%d body=%#v", rr.Code, body)
			}
			attaching := readTaskStreamStatus(t, stream, "attaching")
			if _, leaked := attaching["prompt"]; leaked {
				t.Fatalf("task stream leaked prompt: %#v", attaching)
			}
			select {
			case <-fp.started:
			case <-time.After(time.Second):
				t.Fatal("background Desktop delivery did not start")
			}
			// Model an owner attach that is much slower than the HTTP response.
			time.Sleep(350 * time.Millisecond)
			if got := srv.sendState("codex", "logical-1"); got != "attaching" {
				t.Fatalf("in-flight state=%q, want attaching", got)
			}
			tasks, err := st.Tasks()
			if err != nil || len(tasks) != 1 || recordString(tasks[0], "status") != "attaching" {
				t.Fatalf("tasks=%#v err=%v", tasks, err)
			}

			close(fp.release)
			released = true
			finalTask := readTaskStreamStatus(t, stream, tc.wantStatus)
			if tc.wantStatus == "error" && finalTask["error"] != ownerErr {
				t.Fatalf("error task=%#v", finalTask)
			}
			if got := srv.sendState("codex", "logical-1"); got != "" {
				t.Fatalf("send remained in flight after final event: %q", got)
			}
		})
	}
}

func readTaskStreamStatus(t *testing.T, stream <-chan []byte, wantStatus string) map[string]any {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case payload := <-stream:
			var frame map[string]any
			if err := json.Unmarshal(payload, &frame); err != nil {
				t.Fatal(err)
			}
			if frame["type"] != "task" {
				continue
			}
			task, _ := frame["task"].(map[string]any)
			if task["status"] == wantStatus {
				return task
			}
		case <-deadline:
			t.Fatalf("no task WebSocket event with status %q", wantStatus)
		}
	}
}

func TestSendPromptDirectlyBindsNativeCodexSession(t *testing.T) {
	const threadID = "019f608d-a673-7e70-b276-4734639df599"
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-b", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &directCodexProvider{
		fakePushProvider: fakePushProvider{native: []map[string]any{{
			"native_session_id": threadID, "cli_session_id": threadID,
			"title": "Native thread", "cwd": "/tmp/project",
		}}},
		sentSession: make(chan string, 1),
	}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(`{"provider_id":"codex","session_id":"`+threadID+`","prompt":"direct"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	logicalID := stringAny(body["session_id"])
	if logicalID == "" || logicalID == threadID || stringAny(body["native_session_id"]) != threadID {
		t.Fatalf("direct send did not return logical/native ids: %#v", body)
	}
	select {
	case sentID := <-fp.sentSession:
		if sentID != logicalID {
			t.Fatalf("provider received session %q, want %q", sentID, logicalID)
		}
	case <-time.After(time.Second):
		t.Fatal("direct Codex send was not delivered")
	}
	deadline := time.Now().Add(time.Second)
	for srv.isSendInFlight("codex", logicalID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.isSendInFlight("codex", logicalID) {
		t.Fatal("direct native Codex send did not finish")
	}
	if fp.boundSession != logicalID || fp.boundTranscript != threadID {
		t.Fatalf("binding=(%q,%q), want (%q,%q)", fp.boundSession, fp.boundTranscript, logicalID, threadID)
	}
	if fp.boundRoute != "shared_daemon" || fp.boundCwd != "/tmp/project" {
		t.Fatalf("route binding=(%q,%q), want shared daemon cwd", fp.boundRoute, fp.boundCwd)
	}
	if fp.desktopBound {
		t.Fatal("native Codex preview unexpectedly used Desktop IPC")
	}
	rec, found, err := srv.findSessionForProviderAny("codex", logicalID)
	if err != nil || !found || recordString(rec, "transcript_id") != threadID ||
		recordString(rec, "delivery_route") != "desktop_ipc" ||
		recordString(rec, "codex_control_route") != "shared_daemon" {
		t.Fatalf("persisted session=%#v found=%v err=%v", rec, found, err)
	}

	// A re-opened preview can still submit the native id. It must resolve back
	// to the same logical session rather than creating a second owner.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(`{"provider_id":"codex","session_id":"`+threadID+`","prompt":"again"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("repeat status=%d body=%s", rr.Code, rr.Body.String())
	}
	body = map[string]any{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if stringAny(body["session_id"]) != logicalID {
		t.Fatalf("repeat send session=%q want=%q", stringAny(body["session_id"]), logicalID)
	}
	if fp.desktopBound {
		t.Fatal("reopened native preview unexpectedly changed to Desktop IPC")
	}
	select {
	case sentID := <-fp.sentSession:
		if sentID != logicalID {
			t.Fatalf("repeat provider session=%q want=%q", sentID, logicalID)
		}
	case <-time.After(time.Second):
		t.Fatal("repeat native Codex send was not delivered")
	}
	deadline = time.Now().Add(time.Second)
	for srv.isSendInFlight("codex", logicalID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.isSendInFlight("codex", logicalID) {
		t.Fatal("repeat native Codex send did not finish")
	}
}

func TestSendPromptNativePreviewPreservesExistingAppServerOwner(t *testing.T) {
	tests := []struct {
		name         string
		appRoute     string
		nativeRoute  string
		wantRoute    string
		wantDelivery string
	}{
		{name: "shared daemon", wantRoute: "shared_daemon", wantDelivery: "desktop_ipc"},
		{name: "legacy stdio", appRoute: "stdio", nativeRoute: "desktop_ipc", wantRoute: "stdio"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const threadID = "019f6366-d641-7dd1-bc61-7ba28455d147"
			st := state.New(filepath.Join(t.TempDir(), "data"))
			existing := state.Record{
				"session_id": "logical-app-server", "provider_id": "codex", "title": "Existing",
				"native_session_id": threadID, "transcript_id": threadID, "state": "idle",
			}
			if err := st.UpsertSession(existing); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
			config.ApplyDefaults(cfg)
			fp := &directCodexProvider{
				fakePushProvider: fakePushProvider{native: []map[string]any{{
					"native_session_id": threadID, "cli_session_id": threadID, "title": "Existing",
				}}},
				appServerRoute: tc.appRoute,
				nativeRoute:    tc.nativeRoute,
				sentSession:    make(chan string, 1),
			}
			srv := NewServer(cfg, provider.Registry{"codex": fp}, st)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(`{"provider_id":"codex","session_id":"`+threadID+`","prompt":"direct"}`))
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			select {
			case sentID := <-fp.sentSession:
				if sentID != "logical-app-server" {
					t.Fatalf("provider session=%q", sentID)
				}
			case <-time.After(time.Second):
				t.Fatal("native preview send was not delivered")
			}
			if fp.desktopBound || fp.boundRoute != tc.wantRoute {
				t.Fatalf("native preview owner route=%q desktop=%v", fp.boundRoute, fp.desktopBound)
			}
			rec, found, err := srv.findSessionForProviderAny("codex", "logical-app-server")
			if err != nil || !found || recordString(rec, "delivery_route") != tc.wantDelivery ||
				recordString(rec, "codex_control_route") != tc.wantRoute {
				t.Fatalf("record=%#v found=%v err=%v", rec, found, err)
			}
			deadline := time.Now().Add(time.Second)
			for srv.isSendInFlight("codex", "logical-app-server") && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if srv.isSendInFlight("codex", "logical-app-server") {
				t.Fatal("native preview send did not finish")
			}
		})
	}
}

func TestSendPromptPersistsLegacyDesktopControlRouteBeforeDelivery(t *testing.T) {
	tests := []struct {
		name        string
		nativeRoute string
		wantRoute   string
		wantState   string
		wantDesktop bool
	}{
		{name: "migrates to shared daemon", wantRoute: "shared_daemon", wantState: "delivering"},
		{name: "keeps explicit Desktop compatibility", nativeRoute: "desktop_ipc", wantRoute: "desktop_ipc", wantState: "attaching", wantDesktop: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const threadID = "019f6366-d641-7dd1-bc61-7ba28455d148"
			st := state.New(filepath.Join(t.TempDir(), "data"))
			if err := st.UpsertSession(state.Record{
				"session_id": "legacy-logical", "provider_id": "codex", "title": "Legacy native",
				"native_session_id": threadID, "transcript_id": threadID,
				"delivery_route": "desktop_ipc", "state": "idle",
			}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
			config.ApplyDefaults(cfg)
			fp := &directCodexProvider{nativeRoute: tc.nativeRoute, sentSession: make(chan string, 1)}
			srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(
				`{"provider_id":"codex","session_id":"legacy-logical","prompt":"direct"}`,
			))
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if stringAny(body["state"]) != tc.wantState {
				t.Fatalf("delivery state=%#v, want %q", body, tc.wantState)
			}
			select {
			case sentID := <-fp.sentSession:
				if sentID != "legacy-logical" {
					t.Fatalf("provider session=%q", sentID)
				}
			case <-time.After(time.Second):
				t.Fatal("legacy session prompt was not delivered")
			}
			rec, found, err := srv.findSessionForProviderAny("codex", "legacy-logical")
			if err != nil || !found || recordString(rec, "delivery_route") != "desktop_ipc" ||
				recordString(rec, "codex_control_route") != tc.wantRoute {
				t.Fatalf("record=%#v found=%v err=%v", rec, found, err)
			}
			if fp.desktopBound != tc.wantDesktop || fp.boundRoute != tc.wantRoute {
				t.Fatalf("provider route=%q desktop=%v", fp.boundRoute, fp.desktopBound)
			}
			deadline := time.Now().Add(time.Second)
			for srv.isSendInFlight("codex", "legacy-logical") && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if srv.isSendInFlight("codex", "legacy-logical") {
				t.Fatal("legacy session send did not finish")
			}
		})
	}
}

func TestSendPromptRejectsUnknownNativeCodexSession(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-b", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &directCodexProvider{sentSession: make(chan string, 1)}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, state.New(filepath.Join(t.TempDir(), "data")))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/send_prompt", strings.NewReader(`{"provider_id":"codex","session_id":"missing-thread","prompt":"direct"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-fp.sentSession:
		t.Fatal("unknown native thread was delivered")
	default:
	}
}

func TestTasksCanFilterByTaskID(t *testing.T) {
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.SaveTasks([]state.Record{
		{"task_id": "t1", "session_id": "s1", "provider_id": "codex", "status": "error"},
		{"task_id": "t2", "session_id": "s1", "provider_id": "codex", "status": "running"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"codex": &fakePushProvider{state: "idle"}}, st)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tasks?task_id=t2", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tasks) != 1 || body.Tasks[0]["task_id"] != "t2" {
		t.Fatalf("bad filtered tasks: %#v", body.Tasks)
	}
}

func TestStatusIncludesSessionLastError(t *testing.T) {
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "s1", "provider_id": "codex", "state": "error",
		"last_error": "Desktop IPC timeout waiting for frame",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"codex": &fakePushProvider{state: "idle"}}, st)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status?provider_id=codex&session_id=s1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != "error" || body["last_error"] != "Desktop IPC timeout waiting for frame" {
		t.Fatalf("status did not expose session error: %#v", body)
	}
}

func TestClientVersionsRecordsFetchHeaders(t *testing.T) {
	cfg := &config.Config{
		DeviceID:        "device-a",
		DefaultProvider: "claude",
		Providers:       map[string]config.ProviderConfig{"claude": {}},
	}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.BuildRegistry(cfg), state.New(filepath.Join(t.TempDir(), "data")))
	h := srv.Handler()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status?provider_id=claude&session_id=s1", nil)
	req.Header.Set(clientWebVersionHeader, "web123")
	req.Header.Set(clientIDHeader, "client-a")
	req.Header.Set(clientKindHeader, "web")
	req.Header.Set(clientVisibilityHeader, "visible")
	req.Header.Set("User-Agent", "remote-agent-test")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/client_versions", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("client_versions status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Count   int              `json:"count"`
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count != 1 || len(body.Clients) != 1 {
		t.Fatalf("bad client count: %#v", body)
	}
	row := body.Clients[0]
	if row["client_id"] != "client-a" || row["web_version"] != "web123" || row["last_path"] != "/status" {
		t.Fatalf("bad client row: %#v", row)
	}
	if row["provider_id"] != "claude" || row["session_id"] != "s1" || row["visibility"] != "visible" {
		t.Fatalf("bad scoped fields: %#v", row)
	}
	if _, ok := row["stale"]; ok {
		t.Fatalf("client web version must not be compared with agent version: %#v", row)
	}
}

func TestClientVersionsAcceptsLegacyRemoteCodingHeaders(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.BuildRegistry(cfg), state.New(filepath.Join(t.TempDir(), "data")))
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set(legacyWebVersionHeader, "legacy-web")
	req.Header.Set(legacyIDHeader, "legacy-client")
	req.Header.Set(legacyKindHeader, "legacy")
	req.Header.Set(legacyVisibilityHeader, "visible")
	srv.recordClientVersion(req)
	row := srv.clients["id:legacy-client"]
	if row == nil || row.WebVersion != "legacy-web" || row.Kind != "legacy" || row.Visibility != "visible" {
		t.Fatalf("legacy headers not preserved during migration: %#v", row)
	}
}

func TestClientVersionsRecordsStreamQuery(t *testing.T) {
	cfg := &config.Config{
		DeviceID:        "device-a",
		DefaultProvider: "codex",
		Providers:       map[string]config.ProviderConfig{"codex": {}},
	}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.BuildRegistry(cfg), state.New(filepath.Join(t.TempDir(), "data")))
	h := srv.Handler()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream?provider_id=codex&session_id=s2&web_version=web456&client_id=ws-client&client_kind=web", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("stream status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/client_versions", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("client_versions status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Clients) != 1 {
		t.Fatalf("bad clients: %#v", body.Clients)
	}
	row := body.Clients[0]
	if row["client_id"] != "ws-client" || row["web_version"] != "web456" || row["last_path"] != "/stream" {
		t.Fatalf("bad stream client row: %#v", row)
	}
}

func TestStatusCanBeScopedByProvider(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		DeviceID:        "device-a",
		DefaultProvider: "claude",
		Providers:       map[string]config.ProviderConfig{"claude": {}, "codex": {}},
	}
	config.ApplyDefaults(cfg)
	claude := &fakePushProvider{state: "idle"}
	codex := &fakePushProvider{state: "running"}
	srv := NewServer(cfg, provider.Registry{"claude": claude, "codex": codex}, state.New(filepath.Join(dir, "data")))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status?provider_id=codex&session_id=s1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["active_provider"] != "codex" || body["state"] != "running" || body["active_session_id"] != nil {
		t.Fatalf("status not scoped to codex: %#v", body)
	}
}

func TestSessionsReadsStore(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{"session_id": "s1"}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.BuildRegistry(cfg), st)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string][]map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body["sessions"]) != 1 || body["sessions"][0]["session_id"] != "s1" {
		t.Fatalf("bad sessions: %#v", body)
	}
}

func TestSessionRowsSortByLastReplyThenUpdatedAcrossOffsets(t *testing.T) {
	rows := []map[string]any{
		{
			"native_session_id": "reply-older",
			"last_reply_at":     "2026-07-03T10:00:00+08:00",
			"updated_at":        "2026-07-03T23:00:00Z",
		},
		{
			"native_session_id": "reply-newer",
			"last_reply_at":     "2026-07-03T03:00:00Z",
			"updated_at":        "2026-07-03T01:00:00Z",
		},
		{
			"native_session_id": "updated-fallback",
			"updated_at":        "2026-07-03T04:00:00Z",
		},
	}

	sortSessionRowsNewest(rows)

	got := []string{
		stringAny(rows[0]["native_session_id"]),
		stringAny(rows[1]["native_session_id"]),
		stringAny(rows[2]["native_session_id"]),
	}
	want := []string{"updated-fallback", "reply-newer", "reply-older"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("session order=%v want=%v", got, want)
		}
	}
}

func TestSessionListEndpointsExposeDeviceTimeMetadata(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{
		id:    "codex",
		state: "idle",
		native: []map[string]any{{
			"native_session_id": "thread-1",
			"updated_at":        "2026-07-03T04:00:00Z",
		}},
	}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, state.New(filepath.Join(t.TempDir(), "data")))

	for _, path := range []string{
		"/native_sessions?provider_id=codex&sync=1",
		"/live_sessions?provider_id=codex",
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		var body sessionListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.DeviceTime["device_id"] != "device-a" {
			t.Fatalf("%s missing device identity: %#v", path, body.DeviceTime)
		}
		if _, ok := body.DeviceTime["utc_offset_minutes"].(float64); !ok {
			t.Fatalf("%s missing numeric UTC offset: %#v", path, body.DeviceTime)
		}
		if observed := stringAny(body.DeviceTime["observed_at"]); observed == "" {
			t.Fatalf("%s missing observation time: %#v", path, body.DeviceTime)
		} else if _, err := time.Parse(time.RFC3339Nano, observed); err != nil {
			t.Fatalf("%s bad observation time %q: %v", path, observed, err)
		}
	}
}

func TestLocalTimeZoneIDUsesNamedLocation(t *testing.T) {
	location, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	if got := localTimeZoneID(location); got != "Asia/Kathmandu" {
		t.Fatalf("time zone=%q want Asia/Kathmandu", got)
	}
}

func TestNativeSessionsReturnsStoredSnapshotWhileProviderRefreshBlocks(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "logical-1", "provider_id": "codex", "title": "stored session",
		"transcript_id": "thread-stored", "cwd": "/repo", "updated_at": "2026-07-03T10:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &blockingNativeProvider{
		fakePushProvider: fakePushProvider{state: "idle"},
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		rows: []map[string]any{{
			"cli_session_id": "thread-fresh", "native_session_id": "thread-fresh", "title": "fresh session",
			"cwd": "/repo2", "updated_at": "2026-07-03T10:01:00Z", "live": false,
		}},
	}
	released := false
	defer func() {
		if !released {
			close(fp.release)
		}
	}()
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)
	h := srv.Handler()

	start := time.Now()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex", nil)
	h.ServeHTTP(rr, req)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("native_sessions blocked on provider refresh for %s", elapsed)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows := body["sessions"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["title"] != "stored session" {
		t.Fatalf("did not return stored snapshot: %#v", body)
	}
	select {
	case <-fp.started:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("native refresh was not started in the background")
	}
	close(fp.release)
	released = true
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex", nil)
		h.ServeHTTP(rr, req)
		if strings.Contains(rr.Body.String(), "fresh session") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cache did not publish refreshed native sessions: %s", rr.Body.String())
}

func TestNativeSessionsFailedRefreshPreservesLastGoodSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &scriptedNativeProvider{
		fakePushProvider: fakePushProvider{state: "idle"},
		results: []nativeListResult{
			{rows: []map[string]any{{
				"native_session_id": "thread-old", "title": "last good",
				"updated_at": "2026-07-03T10:00:00Z",
			}}},
			{rows: []map[string]any{{
				"native_session_id": "thread-partial", "title": "partial result",
			}}, err: errors.New("native discovery failed")},
			{panicNow: true},
			{rows: []map[string]any{{
				"native_session_id": "thread-fresh", "title": "fresh result",
				"updated_at": "2026-07-03T10:01:00Z",
			}}},
		},
	}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, state.New(filepath.Join(dir, "data")))
	h := srv.Handler()

	type response struct {
		Sessions     []map[string]any `json:"sessions"`
		Generation   uint64           `json:"generation"`
		RefreshedAt  string           `json:"refreshed_at"`
		Refreshing   bool             `json:"refreshing"`
		RefreshError string           `json:"refresh_error"`
		Meta         map[string]any   `json:"meta"`
	}
	get := func(query string) response {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex&"+query, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var body response
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	first := get("sync=1")
	if first.Generation != 1 || first.RefreshedAt == "" || first.Refreshing || first.RefreshError != "" {
		t.Fatalf("bad initial cache metadata: %#v", first)
	}
	if len(first.Sessions) != 1 || first.Sessions[0]["native_session_id"] != "thread-old" {
		t.Fatalf("bad initial sessions: %#v", first.Sessions)
	}
	if first.Meta["generation"] != float64(1) || first.Meta["has_snapshot"] != true {
		t.Fatalf("bad nested metadata: %#v", first.Meta)
	}

	failed := get("sync=1")
	if failed.Generation != 1 || failed.RefreshedAt != first.RefreshedAt || failed.RefreshError != "native discovery failed" {
		t.Fatalf("failed refresh advanced last-good metadata: %#v", failed)
	}
	if len(failed.Sessions) != 1 || failed.Sessions[0]["native_session_id"] != "thread-old" {
		t.Fatalf("failed refresh replaced last-good sessions: %#v", failed.Sessions)
	}

	panicked := get("sync=1")
	if panicked.Generation != 1 || panicked.RefreshedAt != first.RefreshedAt ||
		panicked.RefreshError != "panic while listing native sessions" {
		t.Fatalf("panic advanced last-good metadata: %#v", panicked)
	}
	if len(panicked.Sessions) != 1 || panicked.Sessions[0]["native_session_id"] != "thread-old" {
		t.Fatalf("panic replaced last-good sessions: %#v", panicked.Sessions)
	}

	recovered := get("include_stale=0")
	if recovered.Generation != 2 || recovered.RefreshError != "" {
		t.Fatalf("successful recovery did not publish a new generation: %#v", recovered)
	}
	if len(recovered.Sessions) != 1 || recovered.Sessions[0]["native_session_id"] != "thread-fresh" {
		t.Fatalf("successful recovery did not replace snapshot: %#v", recovered.Sessions)
	}
}

func TestNativeSessionRefreshIntervalKeepsCodexFastPathScoped(t *testing.T) {
	if got := nativeSessionRefreshInterval("codex"); got != 5*time.Second {
		t.Fatalf("codex refresh interval=%s", got)
	}
	for _, providerID := range []string{"claude", "claude-proxy", "terminal-agent"} {
		if got := nativeSessionRefreshInterval(providerID); got != 15*time.Second {
			t.Fatalf("%s refresh interval=%s", providerID, got)
		}
	}
}

func TestNativeSessionsForceRefreshReturnsImmediatelyAndSingleFlights(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &refreshGateNativeProvider{
		fakePushProvider: fakePushProvider{state: "idle"},
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		initial: []map[string]any{{
			"native_session_id": "thread-old", "title": "last good",
			"updated_at": "2026-07-03T10:00:00Z",
		}},
		fresh: []map[string]any{{
			"native_session_id": "thread-fresh", "title": "fresh result",
			"updated_at": "2026-07-03T10:01:00Z",
		}},
	}
	released := false
	defer func() {
		if !released {
			close(fp.release)
		}
	}()
	srv := NewServer(cfg, provider.Registry{"codex": fp}, state.New(filepath.Join(dir, "data")))
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex&sync=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("initial status=%d body=%s", rr.Code, rr.Body.String())
	}

	type asyncResponse struct {
		code int
		body []byte
	}
	responseCh := make(chan asyncResponse, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex&refresh=1", nil))
		responseCh <- asyncResponse{code: rec.Code, body: rec.Body.Bytes()}
	}()
	var refreshResponse asyncResponse
	select {
	case refreshResponse = <-responseCh:
	case <-time.After(300 * time.Millisecond):
		close(fp.release)
		released = true
		t.Fatal("refresh=1 blocked on provider discovery")
	}
	if refreshResponse.code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", refreshResponse.code, refreshResponse.body)
	}
	var refreshing struct {
		Sessions   []map[string]any `json:"sessions"`
		Generation uint64           `json:"generation"`
		Refreshing bool             `json:"refreshing"`
		Meta       map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal(refreshResponse.body, &refreshing); err != nil {
		t.Fatal(err)
	}
	if refreshing.Generation != 1 || !refreshing.Refreshing ||
		len(refreshing.Sessions) != 1 || refreshing.Sessions[0]["native_session_id"] != "thread-old" {
		t.Fatalf("refresh request did not immediately return last-good snapshot: %#v", refreshing)
	}
	if refreshing.Meta["generation"] != float64(1) || refreshing.Meta["refreshing"] != true {
		t.Fatalf("bad refresh metadata: %#v", refreshing.Meta)
	}
	select {
	case <-fp.started:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("forced refresh was not started")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex&refresh=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("second refresh status=%d body=%s", rr.Code, rr.Body.String())
	}
	if calls := fp.callCount(); calls != 2 {
		t.Fatalf("concurrent force refreshes were not single-flight: calls=%d", calls)
	}

	close(fp.release)
	released = true
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/native_sessions?provider_id=codex", nil))
		var body struct {
			Sessions   []map[string]any `json:"sessions"`
			Generation uint64           `json:"generation"`
			Refreshing bool             `json:"refreshing"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if !body.Refreshing && body.Generation == 2 &&
			len(body.Sessions) == 1 && body.Sessions[0]["native_session_id"] == "thread-fresh" {
			if calls := fp.callCount(); calls != 2 {
				t.Fatalf("refresh completion started an extra discovery: calls=%d", calls)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("forced refresh did not publish fresh generation: %s", rr.Body.String())
}

func TestCreateAndCloseSession(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	workReal, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DeviceID:        "device-a",
		DefaultProvider: "claude",
		Providers: map[string]config.ProviderConfig{
			"claude": {AppName: "Claude Code CLI", Command: "claude", Cwd: work},
		},
	}
	config.ApplyDefaults(cfg)
	st := state.New(filepath.Join(dir, "data"))
	srv := NewServer(cfg, provider.BuildRegistry(cfg), st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"provider_id":"claude","title":"x","cwd":"`+work+`","model":"opus","mode":"edit"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	sess := body["session"].(map[string]any)
	if sess["cwd"] != workReal || sess["model"] != "opus" || sess["mode"] != "edit" {
		t.Fatalf("bad session: %#v", sess)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/close_session", strings.NewReader(`{"session_id":"`+sess["session_id"].(string)+`"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("close status=%d body=%s", rr.Code, rr.Body.String())
	}
	records, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("session not removed: %#v", records)
	}
}

func TestCreateCodexSessionPersistsConcreteOwnerRoute(t *testing.T) {
	tests := []struct {
		name         string
		route        string
		wantDelivery string
	}{
		{name: "shared daemon", route: "shared_daemon", wantDelivery: "desktop_ipc"},
		{name: "legacy stdio", route: "stdio", wantDelivery: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const threadID = "019f608d-a673-7e70-b276-4734639df590"
			st := state.New(filepath.Join(t.TempDir(), "data"))
			cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
			config.ApplyDefaults(cfg)
			fp := &createCodexProvider{
				directCodexProvider: directCodexProvider{
					fakePushProvider: fakePushProvider{id: "codex"},
					appServerRoute:   tc.route,
					sentSession:      make(chan string, 1),
				},
				nativeID: threadID,
			}
			srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"provider_id":"codex","title":"route test","cwd":"/tmp"}`))
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			session, ok := body["session"].(map[string]any)
			if !ok {
				t.Fatalf("response session has unexpected type: %#v", body["session"])
			}
			if recordString(session, "codex_control_route") != tc.route ||
				recordString(session, "delivery_route") != tc.wantDelivery {
				t.Fatalf("response session=%#v", session)
			}
			if fp.boundRoute != tc.route || fp.boundTranscript != threadID {
				t.Fatalf("provider binding route=%q transcript=%q", fp.boundRoute, fp.boundTranscript)
			}
			records, err := st.Sessions()
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%#v err=%v", records, err)
			}
			if recordString(records[0], "codex_control_route") != tc.route ||
				recordString(records[0], "delivery_route") != tc.wantDelivery {
				t.Fatalf("stored session=%#v", records[0])
			}
		})
	}
}

func TestExplicitDesktopControlRouteBindsWithoutLegacyMarker(t *testing.T) {
	fp := &desktopOnlyCodexProvider{fakePushProvider: fakePushProvider{id: "codex"}}
	rec := state.Record{
		"provider_id":         "codex",
		"codex_control_route": "desktop_ipc",
	}
	bindSessionTranscript(fp, rec, "logical", "thread")
	if !fp.desktopBound || fp.boundSession != "logical" || fp.boundTranscript != "thread" {
		t.Fatalf("explicit Desktop route was not authoritative: %#v", fp)
	}
}

func TestResumeNativeCodexSelectsConcreteStdioOwner(t *testing.T) {
	tests := []struct {
		name           string
		persistedRoute string
	}{
		{name: "preserves persisted stdio owner", persistedRoute: "stdio"},
		{name: "migrates route-less app-server owner", persistedRoute: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const threadID = "019f608d-a673-7e70-b276-4734639df591"
			st := state.New(filepath.Join(t.TempDir(), "data"))
			record := state.Record{
				"session_id":        "logical-stdio",
				"provider_id":       "codex",
				"native_session_id": threadID,
				"transcript_id":     threadID,
				"cwd":               "/repo",
			}
			if tc.persistedRoute != "" {
				record["codex_control_route"] = tc.persistedRoute
			}
			if err := st.UpsertSession(record); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
			config.ApplyDefaults(cfg)
			fp := &resumeCodexProvider{directCodexProvider: directCodexProvider{
				fakePushProvider: fakePushProvider{id: "codex", native: []map[string]any{{
					"native_session_id": threadID,
					"cli_session_id":    threadID,
					"title":             "stdio thread",
					"cwd":               "/repo",
				}}},
				appServerRoute: "stdio",
				nativeRoute:    "desktop_ipc",
				sentSession:    make(chan string, 1),
			}}
			srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/resume_native_session", strings.NewReader(
				`{"provider_id":"codex","native_session_id":"`+threadID+`"}`,
			))
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["ok"] != true || fp.resumeSession != "logical-stdio" || fp.resumeID != threadID ||
				fp.boundRoute != "stdio" {
				t.Fatalf("response=%#v provider=%#v", body, fp)
			}
			rec, found, err := srv.findSessionForProviderAny("codex", "logical-stdio")
			if err != nil || !found || recordString(rec, "codex_control_route") != "stdio" ||
				recordString(rec, "delivery_route") != "" {
				t.Fatalf("stored session=%#v found=%v err=%v", rec, found, err)
			}
		})
	}
}

func TestResumeNativeCodexFailsClosedWhenPersistedOwnerCannotBeRead(t *testing.T) {
	const threadID = "019f608d-a673-7e70-b276-4734639df592"
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := state.New(dataDir)
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &resumeCodexProvider{directCodexProvider: directCodexProvider{
		fakePushProvider: fakePushProvider{id: "codex", native: []map[string]any{{
			"native_session_id": threadID,
			"cli_session_id":    threadID,
			"title":             "unreadable owner",
			"cwd":               "/repo",
		}}},
		appServerRoute: "shared_daemon",
		nativeRoute:    "shared_daemon",
		sentSession:    make(chan string, 1),
	}}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/resume_native_session", strings.NewReader(
		`{"provider_id":"codex","native_session_id":"`+threadID+`"}`,
	))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if fp.resumeSession != "" || fp.resumeID != "" || fp.boundRoute != "" {
		t.Fatalf("unreadable owner reached provider: %#v", fp)
	}
}

func TestBrowseDirsAndProjectFile(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "README.md")
	if err := os.WriteFile(file, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", ProjectRoots: []string{dir}, Providers: map[string]config.ProviderConfig{"claude": {Cwd: root}}}
	config.ApplyDefaults(cfg)
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{"session_id": "s1", "provider_id": "claude", "cwd": root}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, provider.BuildRegistry(cfg), st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/browse_dirs?path="+url.QueryEscape(root), nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("browse status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"sub"`) {
		t.Fatalf("missing sub dir: %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/project_file?session_id=s1&path=README.md", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("file status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"content":"hello\n"`) {
		t.Fatalf("bad file body: %s", rr.Body.String())
	}
}

func TestRewindUserMessageCreatesDrivingSession(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "old-logical", "provider_id": "codex", "title": "Inspect",
		"cwd": "/repo", "native_session_id": "thread-1", "transcript_id": "thread-1",
		"delivery_route": "desktop_ipc", "codex_control_route": "shared_daemon",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakeRewindProvider{}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rewind_user_message", strings.NewReader(`{"provider_id":"codex","session_id":"thread-1","turn_id":"turn-b","prompt":"edited","title":"Inspect"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["thread_id"] != "thread-1" || body["provider_id"] != "codex" {
		t.Fatalf("bad body: %#v", body)
	}
	if fp.opts.ThreadID != "thread-1" || fp.opts.TurnID != "turn-b" || fp.opts.Prompt != "edited" || fp.opts.Cwd != "/repo" {
		t.Fatalf("bad provider opts: %#v", fp.opts)
	}
	records, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	var created state.Record
	for _, rec := range records {
		if recordString(rec, "session_id") == body["session_id"] {
			created = rec
			break
		}
	}
	if created == nil || recordString(created, "transcript_id") != "thread-1" ||
		recordString(created, "last_prompt") != "edited" ||
		recordString(created, "codex_control_route") != "shared_daemon" ||
		recordString(created, "delivery_route") != "desktop_ipc" {
		t.Fatalf("rewind session not stored: body=%#v records=%#v", body, records)
	}
}

func TestRewindUserMessageUnsupportedProvider(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"claude": &fakePushProvider{state: "idle"}}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rewind_user_message", strings.NewReader(`{"provider_id":"claude","session_id":"claude-1","turn_id":"turn-b","prompt":"edited"}`))
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLiveSessionsUseRuntimeSourceNotPersistedRunningState(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{
		{"session_id": "stale-codex", "provider_id": "codex", "state": "running", "title": "stale"},
		{"session_id": "live-claude", "provider_id": "claude", "state": "running", "title": "live", "transcript_id": "tid1"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}, "codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{state: "idle", live: []map[string]any{{"session_id": "live-claude", "tmux_session": "rc-claude-live-claude"}}}
	srv := NewServer(cfg, provider.Registry{"claude": fp, "codex": &fakePushProvider{id: "codex"}}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("live sessions=%#v", body.Sessions)
	}
	if body.Sessions[0]["session_id"] != "live-claude" || body.Sessions[0]["provider_id"] != "claude" {
		t.Fatalf("bad live session: %#v", body.Sessions[0])
	}
}

func TestLiveSessionsPreserveRuntimeTranscriptWithoutRecord(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{state: "idle", live: []map[string]any{{
		"session_id": "logical-1", "provider_id": "codex", "native_session_id": "thread-1",
		"transcript_id": "thread-1", "cwd": "/repo", "updated_at": "2026-06-24T10:00:00Z",
	}}}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("live sessions=%#v", body.Sessions)
	}
	row := body.Sessions[0]
	if row["transcript_id"] != "thread-1" || row["cwd"] != "/repo" || row["updated_at"] != "2026-06-24T10:00:00Z" {
		t.Fatalf("runtime fields not preserved: %#v", row)
	}
	if row["stored"] != false {
		t.Fatalf("runtime-only session should not be marked stored: %#v", row)
	}
}

func TestLiveSessionsSkipsRuntimeRowsMarkedNotLiveByDefault(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{state: "idle", live: []map[string]any{{
		"session_id": "thread-1", "provider_id": "codex", "native_session_id": "thread-1",
		"transcript_id": "thread-1", "title": "loaded but idle", "live": false,
	}}}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions?provider_id=codex", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 0 {
		t.Fatalf("runtime rows marked live=false should be hidden by default: %#v", body.Sessions)
	}
}

func TestLiveSessionsMapsRuntimeTranscriptToStoredLogicalSession(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "logical-1", "provider_id": "claude", "title": "stored",
		"transcript_id": "transcript-1", "cwd": "/repo", "updated_at": "2026-06-20T10:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{state: "idle", live: []map[string]any{{
		"session_id": "transcript-1", "provider_id": "claude", "native_session_id": "transcript-1",
		"transcript_id": "transcript-1", "title": "runtime", "live": true, "updated_at": "2026-06-24T10:00:00Z",
	}}}
	srv := NewServer(cfg, provider.Registry{"claude": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("live sessions=%#v", body.Sessions)
	}
	row := body.Sessions[0]
	if row["session_id"] != "logical-1" || row["transcript_id"] != "transcript-1" || row["title"] != "stored" || row["cwd"] != "/repo" {
		t.Fatalf("runtime transcript not mapped to stored session: %#v", row)
	}
	if row["updated_at"] != "2026-06-24T10:00:00Z" {
		t.Fatalf("runtime updated_at should win over stored record: %#v", row)
	}
	if row["stored"] != true {
		t.Fatalf("stored session should be marked stored: %#v", row)
	}
}

func TestLiveSessionsSkipsInactiveNativeSessionsByDefault(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{state: "idle", native: []map[string]any{{
		"cli_session_id": "thread-1", "native_session_id": "thread-1", "title": "inactive",
		"cwd": "/repo", "updated_at": "2026-06-24T10:00:00Z", "status": "notLoaded", "live": false,
	}}}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions?provider_id=codex", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 0 {
		t.Fatalf("inactive native sessions should be hidden by default: %#v", body.Sessions)
	}
}

func TestLiveSessionsIncludesInactiveNativeSessions(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{state: "idle", native: []map[string]any{{
		"cli_session_id": "thread-1", "native_session_id": "thread-1", "title": "inactive",
		"cwd": "/repo", "updated_at": "2026-06-24T10:00:00Z", "status": "notLoaded", "live": false,
	}}}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions?provider_id=codex&include_inactive=1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("sessions=%#v", body.Sessions)
	}
	row := body.Sessions[0]
	if row["transcript_id"] != "thread-1" || row["title"] != "inactive" || row["live"] != false || row["status"] != "notLoaded" {
		t.Fatalf("bad inactive native row: %#v", row)
	}
	if row["stored"] != false {
		t.Fatalf("inactive native-only session should not be marked stored: %#v", row)
	}
}

func TestLiveSessionsUsesStoredSnapshotWhenNativeRefreshBlocks(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "logical-1", "provider_id": "codex", "title": "stored inactive",
		"transcript_id": "thread-stored", "cwd": "/repo", "updated_at": "2026-07-03T10:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &blockingNativeProvider{
		fakePushProvider: fakePushProvider{state: "idle"},
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		rows:             []map[string]any{},
	}
	defer close(fp.release)
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	start := time.Now()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions?provider_id=codex&include_inactive=1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("live_sessions blocked on native refresh for %s", elapsed)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0]["title"] != "stored inactive" {
		t.Fatalf("did not use stored snapshot: %#v", body.Sessions)
	}
}

func TestLiveSessionsIncludeInactivePromotesDuplicateLiveNative(t *testing.T) {
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "data"))
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakePushProvider{
		state: "idle",
		live: []map[string]any{{
			"session_id": "thread-1", "provider_id": "codex", "native_session_id": "thread-1",
			"transcript_id": "thread-1", "title": "runtime idle", "live": false,
			"updated_at": "2026-06-24T09:00:00Z", "last_reply_at": "2026-06-24T08:00:00Z",
		}},
		native: []map[string]any{{
			"cli_session_id": "thread-1", "native_session_id": "thread-1", "title": "native live",
			"cwd": "/repo", "updated_at": "2026-06-24T10:00:00Z", "last_reply_at": "2026-06-24T11:00:00Z",
			"status": "active", "live": true,
		}},
	}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/live_sessions?provider_id=codex&include_inactive=1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("sessions=%#v", body.Sessions)
	}
	row := body.Sessions[0]
	if row["live"] != true || row["status"] != "active" || row["state"] != "running" ||
		row["updated_at"] != "2026-06-24T10:00:00Z" || row["last_reply_at"] != "2026-06-24T11:00:00Z" {
		t.Fatalf("duplicate live native row should promote inactive runtime row: %#v", row)
	}
}

type fakeRewindProvider struct {
	fakePushProvider
	opts provider.RewindUserMessageOptions
}

func (f *fakeRewindProvider) ID() string { return "codex" }
func (f *fakeRewindProvider) AppServerDeliveryRoute() string {
	return "shared_daemon"
}

func (f *fakeRewindProvider) RewindUserMessage(opts provider.RewindUserMessageOptions) (provider.RewindUserMessageResult, error) {
	f.opts = opts
	return provider.RewindUserMessageResult{
		SessionID:    opts.SessionID,
		ThreadID:     opts.ThreadID,
		TurnID:       "turn-edited",
		State:        "running",
		NativeTaskID: opts.ThreadID,
	}, nil
}

type fakeControlProvider struct {
	fakePushProvider
	sessionID       string
	requestID       string
	decision        string
	answers         map[string]string
	boundSession    string
	boundTranscript string
}

func (f *fakeControlProvider) BindTranscript(sessionID string, transcriptID string) {
	f.boundSession, f.boundTranscript = sessionID, transcriptID
}

func (f *fakeControlProvider) RelayApprovalRequest(sessionID string, requestID string, decision string) map[string]any {
	f.sessionID, f.requestID, f.decision = sessionID, requestID, decision
	return map[string]any{"ok": true, "status": "responding", "confirmed": false, "detail": "written"}
}

func (f *fakeControlProvider) AnswerQuestion(sessionID string, requestID string, answers map[string]string) map[string]any {
	f.sessionID, f.requestID, f.answers = sessionID, requestID, answers
	return map[string]any{"ok": true, "detail": "answered"}
}

func (f *fakeControlProvider) SessionRunning(string) *bool {
	running := true
	return &running
}

func (f *fakeControlProvider) ApprovalRequest(string) map[string]any {
	return map[string]any{"type": "command", "request_id": "request-1"}
}

func (f *fakeControlProvider) PendingApprovalSessionIDs() []string {
	return []string{"session-1"}
}

func TestRequestScopedApprovalWithoutTask(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "claude", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakeControlProvider{fakePushProvider: fakePushProvider{state: "waiting_approval"}}
	srv := NewServer(cfg, provider.Registry{"claude": fp}, state.New(filepath.Join(t.TempDir(), "data")))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/approval", strings.NewReader(`{
		"provider_id":"claude","session_id":"session-1","request_id":"request-1","decision":"allow"
	}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || fp.sessionID != "session-1" || fp.requestID != "request-1" || fp.decision != "allow" {
		t.Fatalf("status=%d body=%s provider=%#v", rr.Code, rr.Body.String(), fp)
	}
	var approvalResponse map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &approvalResponse); err != nil {
		t.Fatal(err)
	}
	if approvalResponse["status"] != "responding" || truthy(approvalResponse["confirmed"], false) {
		t.Fatalf("approval source-confirmation state was lost: %#v", approvalResponse)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/status?provider_id=claude&session_id=session-1", nil)
	srv.Handler().ServeHTTP(rr, req)
	var status map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["state"] != "waiting_approval" {
		t.Fatalf("running flag hid pending approval: %#v", status)
	}
}

func TestApprovalHydratesStoredMappingAfterRestart(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "claude", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.UpsertSession(state.Record{
		"session_id": "r-claude-stored", "provider_id": "claude", "transcript_id": "native-desktop-1",
	}); err != nil {
		t.Fatal(err)
	}
	fp := &fakeControlProvider{fakePushProvider: fakePushProvider{state: "waiting_approval"}}
	srv := NewServer(cfg, provider.Registry{"claude": fp}, st)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/approval", strings.NewReader(`{
		"provider_id":"claude","session_id":"r-claude-stored","request_id":"request-1","decision":"allow"
	}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || fp.boundSession != "r-claude-stored" || fp.boundTranscript != "native-desktop-1" {
		t.Fatalf("stored mapping not hydrated: status=%d body=%s provider=%#v", rr.Code, rr.Body.String(), fp)
	}
}

func TestApprovalRejectsTaskProviderOrSessionMismatch(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "claude", Providers: map[string]config.ProviderConfig{"claude": {}, "codex": {}}}
	config.ApplyDefaults(cfg)
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.AppendTask(state.Record{
		"task_id": "task-codex", "provider_id": "codex", "session_id": "codex-session", "status": "waiting_approval",
	}); err != nil {
		t.Fatal(err)
	}
	fp := &fakeControlProvider{fakePushProvider: fakePushProvider{state: "waiting_approval"}}
	srv := NewServer(cfg, provider.Registry{"claude": fp, "codex": &fakePushProvider{id: "codex"}}, st)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/approval", strings.NewReader(`{
		"task_id":"task-codex","provider_id":"claude","session_id":"claude-session","decision":"allow"
	}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict || fp.decision != "" {
		t.Fatalf("mismatched task reached provider: status=%d body=%s provider=%#v", rr.Code, rr.Body.String(), fp)
	}
}

func TestStatusPendingRequestOverridesStaleProviderState(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "claude", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakeControlProvider{fakePushProvider: fakePushProvider{state: "idle"}}
	st := state.New(filepath.Join(t.TempDir(), "data"))
	if err := st.SaveSessions([]state.Record{{
		"session_id": "native-session-1", "provider_id": "claude", "state": "error", "last_error": "stale error",
	}}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, provider.Registry{"claude": fp}, st)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status?provider_id=claude&session_id=native-session-1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	request, _ := body["approval_request"].(map[string]any)
	if body["state"] != "waiting_approval" || request["request_id"] != "request-1" {
		t.Fatalf("pending request did not override stale state: %#v", body)
	}
}

type fakeBindingProvider struct {
	fakePushProvider
	sessionID    string
	transcriptID string
}

func (f *fakeBindingProvider) BindTranscript(sessionID string, transcriptID string) {
	f.sessionID, f.transcriptID = sessionID, transcriptID
}

func TestStatusBindsUnstoredNativePreview(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "codex", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakeBindingProvider{fakePushProvider: fakePushProvider{state: "idle"}}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, state.New(filepath.Join(t.TempDir(), "data")))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status?provider_id=codex&session_id=native-thread-1", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || fp.sessionID != "native-thread-1" || fp.transcriptID != "native-thread-1" {
		t.Fatalf("native preview was not bound: status=%d provider=%#v body=%s", rr.Code, fp, rr.Body.String())
	}
}

func TestWebPreviewTracksNativeSessionApprovals(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "static", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	ui := string(b)
	if got := strings.Count(ui, "LIVE.sessionId || LIVE.resumeId || LIVE.tid"); got < 4 {
		t.Fatalf("native preview scope must cover status and approval actions, occurrences=%d", got)
	}
	for _, want := range []string{
		"LIVE.sessionId || LIVE.resumeId || LIVE.tid",
		"const streamId = tab.tid || tab.sessionId || tab.resumeId",
		`f.type === "approval_changed"`,
		`(t.device || "") + "|" + (t.provider || "")`,
		`const tabUrl = (tab, p) => deviceBase(tab && tab.device)`,
		`rememberShellDevice(CUR_DEVICE, t.provider || CUR_PROVIDER, true)`,
		`setActive("", "No session");
      persistTabs();`,
		`pending_approvals`,
		`CURRENT_APPROVAL = null; window.__taskId = null`,
		`?provider_id=${encodeURIComponent(prov)}&session_id=${encodeURIComponent(sid)}`,
		`ar.actionable === false`,
	} {
		if !strings.Contains(ui, want) {
			t.Fatalf("native-session approval tracking missing %q", want)
		}
	}
}

func TestPendingInteractionState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request map[string]any
		want    string
	}{
		{name: "approval", request: map[string]any{"type": "approval", "actionable": true}, want: "waiting_approval"},
		{name: "question", request: map[string]any{"type": "question", "actionable": true}, want: "waiting_input"},
		{name: "native ui", request: map[string]any{"type": "native_ui", "actionable": false}, want: "waiting_input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pendingInteractionState(tc.request); got != tc.want {
				t.Fatalf("pendingInteractionState(%#v)=%q want %q", tc.request, got, tc.want)
			}
		})
	}
}

func TestClaudeProviderAliasesUseSameLogicalID(t *testing.T) {
	native := "019f5105-b411-7a22-8838-d13e30edc1ca"
	desktop := providerScopedLogicalID("claude", native)
	cli := providerScopedLogicalID("claude_cli", native)
	legacyDesktop := providerScopedLogicalID("claude_desktop", native)
	if desktop != cli || desktop != legacyDesktop || !strings.HasPrefix(desktop, "r-claude-") {
		t.Fatalf("Claude aliases split one transcript: %q %q %q", desktop, cli, legacyDesktop)
	}
}

func TestQuestionAnswerEndpoint(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "claude", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	fp := &fakeControlProvider{}
	srv := NewServer(cfg, provider.Registry{"claude": fp}, state.New(filepath.Join(t.TempDir(), "data")))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/question_answer", strings.NewReader(`{
		"provider_id":"claude","session_id":"session-1","request_id":"question-1",
		"answers":{"Which mode?":"Safe"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || fp.sessionID != "session-1" || fp.requestID != "question-1" || fp.answers["Which mode?"] != "Safe" {
		t.Fatalf("status=%d body=%s provider=%#v", rr.Code, rr.Body.String(), fp)
	}
}

// fakeIdleAwareProvider reports controllable per-session running state so
// task reconciliation can be exercised.
type fakeIdleAwareProvider struct {
	fakePushProvider
	running *bool
}

func (f *fakeIdleAwareProvider) SessionRunning(string) *bool { return f.running }

func TestTasksReconcileWithSessionRuntime(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "codex", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	st := state.New(filepath.Join(t.TempDir(), "data"))
	notRunning := false
	fp := &fakeIdleAwareProvider{fakePushProvider: fakePushProvider{state: "idle"}, running: &notRunning}
	srv := NewServer(cfg, provider.Registry{"codex": fp}, st)

	task := newTaskRecord("device-a", "sess-1", "codex", "do something")
	task["status"] = "running"
	if err := st.AppendTask(task); err != nil {
		t.Fatal(err)
	}

	fetch := func() map[string]any {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/tasks?session_id=sess-1", nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var body map[string][]map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body["tasks"]) != 1 {
			t.Fatalf("tasks=%#v", body["tasks"])
		}
		return body["tasks"][0]
	}

	// Turn completed + session idle: the running task converges to completed.
	if got := fetch(); got["status"] != "completed" {
		t.Fatalf("running task must converge after turn completion: %#v", got)
	}

	// A fresh task while the session waits on an approval surfaces as
	// waiting_approval instead of silently running forever.
	task2 := newTaskRecord("device-a", "sess-1", "codex", "next")
	task2["status"] = "running"
	if err := st.AppendTask(task2); err != nil {
		t.Fatal(err)
	}
	fp.state = "waiting_approval"
	running := true
	fp.running = &running
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tasks?session_id=sess-1&status=waiting_approval", nil)
	srv.Handler().ServeHTTP(rr, req)
	var body map[string][]map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body["tasks"]) != 1 || body["tasks"][0]["task_id"] != task2["task_id"] {
		t.Fatalf("waiting_approval reconcile failed: %#v", body["tasks"])
	}
}

// fakeInstallProvider reports controllable install state so /providers
// hiding can be exercised.
type fakeInstallProvider struct {
	fakePushProvider
	installed bool
}

func (f *fakeInstallProvider) Installed() bool { return f.installed }

func TestProvidersHideUninstalled(t *testing.T) {
	cfg := &config.Config{DeviceID: "device-a", DefaultProvider: "codex", Providers: map[string]config.ProviderConfig{"codex": {}, "claude": {}}}
	config.ApplyDefaults(cfg)
	reg := provider.Registry{
		"codex":  &fakeInstallProvider{installed: false},
		"claude": &fakeInstallProvider{installed: true},
	}
	srv := NewServer(cfg, reg, state.New(filepath.Join(t.TempDir(), "data")))

	fetch := func(path string) map[string]any {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := fetch("/providers")
	rows := body["providers"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["provider_id"] != "claude" {
		t.Fatalf("uninstalled provider must be hidden: %#v", rows)
	}
	// The configured default (codex) is hidden, so active_provider falls
	// back to an installed one for the UI.
	if body["active_provider"] != "claude" {
		t.Fatalf("active_provider must fall back to an installed provider: %v", body["active_provider"])
	}
	row := rows[0].(map[string]any)
	if actions, ok := row["actions"].([]any); !ok || len(actions) == 0 {
		t.Fatalf("typed provider actions missing: %#v", row)
	}
	body = fetch("/providers?include_uninstalled=1")
	if rows := body["providers"].([]any); len(rows) != 2 {
		t.Fatalf("include_uninstalled must list everything: %#v", rows)
	}
	if body["active_provider"] != "codex" {
		t.Fatalf("include_uninstalled keeps the real active provider: %v", body["active_provider"])
	}
}
