package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

func TestWebPushEncryptRoundTrip(t *testing.T) {
	receiver, err := ecdsa.GenerateKey(elliptic.P256(), zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := ecdsa.GenerateKey(elliptic.P256(), fixedReader{b: 7})
	if err != nil {
		t.Fatal(err)
	}
	receiverPub := elliptic.Marshal(elliptic.P256(), receiver.PublicKey.X, receiver.PublicKey.Y)
	auth := []byte("0123456789abcdef")
	salt := []byte("abcdefghijklmnop")
	plain := []byte(`{"title":"待批准","body":"run command"}`)

	body, err := encryptWebPushWith(plain, receiverPub, receiver.PublicKey.X, receiver.PublicKey.Y, auth, sender, salt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptWebPushForTest(body, receiver, auth)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("decrypted=%s want %s", got, plain)
	}
}

func TestNewWebPushRequestBuildsEncryptedRequest(t *testing.T) {
	_, p256dh, auth := receiverSubscriptionForTest(t)
	keys, err := generateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	req, err := newWebPushRequest(pushSub{Endpoint: "https://push.example.test/send", P256DH: p256dh, Auth: auth}, keys, []byte(`{"title":"t","body":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost {
		t.Fatalf("method=%s", req.Method)
	}
	if req.Header.Get("Content-Encoding") != "aes128gcm" {
		t.Fatalf("bad content encoding: %s", req.Header.Get("Content-Encoding"))
	}
	if req.Header.Get("TTL") != "86400" || req.Header.Get("Urgency") != "high" {
		t.Fatalf("bad push headers: %#v", req.Header)
	}
	if req.Header.Get("Authorization") == "" || req.Header.Get("Crypto-Key") == "" {
		t.Fatalf("missing VAPID headers: %#v", req.Header)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 16+4+1+65 {
		t.Fatalf("encrypted body too short: %d", len(body))
	}
}

func TestPushMonitorEdgesAndPresence(t *testing.T) {
	fp := &fakePushProvider{state: "idle", approval: "run ls"}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"claude": fp}, state.New(filepath.Join(t.TempDir(), "data")))
	srv.activeProvider = "claude"
	srv.activeSessionID = stringPtr("s1")
	sent := []map[string]any{}
	srv.pushSender = func(payload map[string]any) int {
		sent = append(sent, payload)
		return 1
	}

	srv.pushMonitorTick()
	fp.state = "waiting_approval"
	srv.pushMonitorTick()
	srv.pushMonitorTick()
	if len(sent) != 1 || sent[0]["kind"] != "approval" || sent[0]["session"] != "s1" {
		t.Fatalf("bad edge sends: %#v", sent)
	}

	fp.state = "running"
	srv.pushMonitorTick()
	srv.presence["s1"] = time.Now()
	fp.state = "waiting_approval"
	srv.pushMonitorTick()
	if len(sent) != 1 {
		t.Fatalf("presence did not suppress send: %#v", sent)
	}
}

func decryptWebPushForTest(body []byte, receiver *ecdsa.PrivateKey, auth []byte) ([]byte, error) {
	salt := body[:16]
	rs := int(binary.BigEndian.Uint32(body[16:20]))
	keyLen := int(body[20])
	senderPub := body[21 : 21+keyLen]
	content := body[21+keyLen:]
	sx, sy := elliptic.Unmarshal(elliptic.P256(), senderPub)
	receiverPub := elliptic.Marshal(elliptic.P256(), receiver.PublicKey.X, receiver.PublicKey.Y)
	sharedX, _ := elliptic.P256().ScalarMult(sx, sy, leftPad(receiver.D.Bytes(), 32))
	shared := leftPad(sharedX.Bytes(), 32)
	context := append([]byte("WebPush: info\x00"), receiverPub...)
	context = append(context, senderPub...)
	ikm := hkdfExpand(hkdfExtract(auth, shared), context, 32)
	prk := hkdfExtract(salt, ikm)
	cek := hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	out := []byte{}
	var counter uint64
	for off := 0; off < len(content); off += rs {
		end := off + rs
		if end > len(content) {
			end = len(content)
		}
		record, err := aead.Open(nil, webPushIV(nonce, counter), content[off:end], nil)
		if err != nil {
			return nil, err
		}
		if len(record) > 0 {
			out = append(out, record[:len(record)-1]...)
		}
		counter++
	}
	return out, nil
}

func receiverSubscriptionForTest(t *testing.T) (*ecdsa.PrivateKey, string, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), fixedReader{b: 3})
	if err != nil {
		t.Fatal(err)
	}
	pub := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	auth := []byte("auth-secret-1234")
	return priv, b64URLNoPad(pub), b64URLNoPad(auth)
}

type fixedReader struct {
	b byte
}

func (r fixedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i + 1)
	}
	return len(p), nil
}

type fakePushProvider struct {
	id       string
	state    string
	approval string
	requests map[string]map[string]any
	live     []map[string]any
	native   []map[string]any
}

func (f *fakePushProvider) ID() string {
	if f.id != "" {
		return f.id
	}
	return "claude"
}
func (f *fakePushProvider) Status() provider.Status {
	return provider.Status{ProviderID: f.ID(), IsRunning: true, State: f.state}
}
func (f *fakePushProvider) ModelSelect() provider.ModelSelect { return provider.ModelSelect{} }
func (f *fakePushProvider) ListNativeSessions() []map[string]any {
	return f.native
}
func (f *fakePushProvider) SessionMessages(string) ([]map[string]any, error) { return nil, nil }
func (f *fakePushProvider) SessionModel(string) map[string]any               { return nil }
func (f *fakePushProvider) ReferencedFiles(string) map[string]bool           { return map[string]bool{} }
func (f *fakePushProvider) OpenOrCreateSession(string, provider.StartOptions) (string, error) {
	return "", nil
}
func (f *fakePushProvider) CloseSession(string) map[string]any {
	return map[string]any{"ok": true}
}
func (f *fakePushProvider) SendPrompt(string, string) provider.SendResult {
	return provider.SendResult{OK: true}
}
func (f *fakePushProvider) LatestOutput(string) map[string]any {
	return map[string]any{"text": f.approval}
}
func (f *fakePushProvider) DetectState(string) string { return f.state }
func (f *fakePushProvider) ApprovalRequest(sessionID string) map[string]any {
	if f.requests != nil {
		return f.requests[sessionID]
	}
	if f.state != "waiting_approval" {
		return nil
	}
	return map[string]any{"type": "approval", "summary": f.approval}
}
func (f *fakePushProvider) RelayApproval(string, string) map[string]any {
	return map[string]any{"ok": true}
}
func (f *fakePushProvider) SendKeys(string, []string) map[string]any {
	return map[string]any{"ok": true}
}
func (f *fakePushProvider) Interrupt(string) map[string]any {
	return map[string]any{"ok": true}
}
func (f *fakePushProvider) SetSessionModel(string, string, string) map[string]any {
	return map[string]any{"ok": true}
}
func (f *fakePushProvider) RuntimeSessions() []map[string]any { return f.live }

func TestPushPayloadJSON(t *testing.T) {
	payload := buildPushPayload("s1", "waiting_input", "hello")
	b, err := json.Marshal(payload)
	if err != nil || !json.Valid(b) {
		t.Fatalf("bad payload: %s %v", b, err)
	}
}

func TestPushStatesIncludeBackgroundProviderSessions(t *testing.T) {
	claude := &fakePushProvider{id: "claude", live: []map[string]any{
		{"session_id": "desktop-1", "transcript_id": "desktop-1"},
	}, requests: map[string]map[string]any{
		"desktop-1": {"type": "native_ui", "request_id": "claude-request", "summary": "native", "actionable": false},
	}}
	codex := &fakePushProvider{id: "codex", live: []map[string]any{
		{"session_id": "codex-1", "transcript_id": "codex-1"},
	}, requests: map[string]map[string]any{
		"codex-1": {"type": "permissions", "request_id": "codex-request", "summary": "permissions"},
	}}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"claude": {}, "codex": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"claude": claude, "codex": codex}, state.New(filepath.Join(t.TempDir(), "data")))
	srv.activeProvider = "codex"
	srv.activeSessionID = stringPtr("codex-1")
	states := srv.pushStates()
	if len(states) != 2 {
		t.Fatalf("background approvals missing: %#v", states)
	}
	seen := map[string]bool{}
	for _, row := range states {
		seen[approvalIdentity(row.ProviderID, row.NativeSessionID)] = true
	}
	if !seen["claude|desktop-1"] || !seen["codex|codex-1"] {
		t.Fatalf("provider-scoped identities missing: %#v", seen)
	}
	for _, row := range states {
		if row.ProviderID == "claude" && row.State != "waiting_input" {
			t.Fatalf("non-actionable native UI request should be waiting_input: %#v", row)
		}
	}
}

func TestPushMonitorUsesRequestIdentityAndResetsAfterResolution(t *testing.T) {
	fp := &fakePushProvider{id: "codex", live: []map[string]any{{"session_id": "thread-1", "transcript_id": "thread-1"}}, requests: map[string]map[string]any{
		"thread-1": {"type": "command", "request_id": "request-1", "summary": "first"},
	}}
	cfg := &config.Config{DeviceID: "device-a", Providers: map[string]config.ProviderConfig{"codex": {}}}
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"codex": fp}, state.New(filepath.Join(t.TempDir(), "data")))
	sent := []map[string]any{}
	srv.pushSender = func(payload map[string]any) int { sent = append(sent, payload); return 1 }
	srv.pushMonitorTick()
	fp.requests["thread-1"] = map[string]any{"type": "command", "request_id": "request-2", "summary": "second"}
	srv.pushMonitorTick()
	delete(fp.requests, "thread-1")
	srv.pushMonitorTick()
	fp.requests["thread-1"] = map[string]any{"type": "command", "summary": "third"}
	srv.pushMonitorTick()
	if len(sent) != 3 {
		t.Fatalf("request edges not preserved: %#v", sent)
	}
	if sent[1]["request_id"] != "request-2" || sent[1]["provider"] != "codex" || sent[1]["native_session"] != "thread-1" {
		t.Fatalf("request identity missing: %#v", sent[1])
	}
}
