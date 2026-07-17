package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	pushVAPIDSub      = "mailto:remote-coding@example.invalid"
	pushFailThreshold = 5
	pushPresenceTTL   = 25 * time.Second
	pushMonitorEvery  = 2 * time.Second
	pushBodyLimit     = 140
)

var pushActionableStates = map[string]bool{
	"waiting_approval": true,
	"waiting_input":    true,
}

type pushSub struct {
	Endpoint  string `json:"endpoint"`
	P256DH    string `json:"p256dh"`
	Auth      string `json:"auth"`
	FailCount int    `json:"fail_count"`
}

type pushState struct {
	ProviderID      string
	SessionID       string
	NativeSessionID string
	RequestID       string
	State           string
	Context         string
}

func (s *Server) pushVAPID(w http.ResponseWriter, r *http.Request) {
	keys, err := s.loadOrCreateVAPID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"public_key": keys["public_b64"]})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Endpoint string            `json:"endpoint"`
		Keys     map[string]string `json:"keys"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Endpoint == "" || body.Keys["p256dh"] == "" || body.Keys["auth"] == "" {
		writeError(w, http.StatusBadRequest, "missing endpoint/keys")
		return
	}
	subs, err := s.loadPushSubs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	replaced := false
	for i := range subs {
		if subs[i].Endpoint == body.Endpoint {
			subs[i].P256DH = body.Keys["p256dh"]
			subs[i].Auth = body.Keys["auth"]
			subs[i].FailCount = 0
			replaced = true
			break
		}
	}
	if !replaced {
		subs = append(subs, pushSub{Endpoint: body.Endpoint, P256DH: body.Keys["p256dh"], Auth: body.Keys["auth"]})
	}
	if err := s.savePushSubs(subs); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": endpointHash(body.Endpoint)})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Endpoint != "" {
		if err := s.removePushSub(body.Endpoint); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) pushPresence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ProviderID string `json:"provider_id"`
		SessionID  string `json:"session_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.SessionID != "" {
		s.presenceMu.Lock()
		s.presence[approvalIdentity(body.ProviderID, body.SessionID)] = time.Now()
		s.presenceMu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) pushApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		ProviderID      string `json:"provider_id"`
		SessionID       string `json:"session_id"`
		NativeSessionID string `json:"native_session_id"`
		RequestID       string `json:"request_id"`
		Decision        string `json:"decision"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Decision != "allow" && body.Decision != "deny" {
		writeError(w, http.StatusBadRequest, "decision must be 'allow' or 'deny'")
		return
	}
	providerID := body.ProviderID
	if tasks, err := s.store.Tasks(); err == nil {
		for i := len(tasks) - 1; providerID == "" && i >= 0; i-- {
			if recordString(tasks[i], "session_id") == body.SessionID {
				providerID = recordString(tasks[i], "provider_id")
				break
			}
		}
	}
	p, providerID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "provider has no approval relay")
		return
	}
	sessionID := firstNonEmpty(body.SessionID, body.NativeSessionID)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if err := s.hydrateControlSession(p, providerID, sessionID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var relay map[string]any
	if body.RequestID != "" {
		if relayer, ok := p.(interface {
			RelayApprovalRequest(string, string, string) map[string]any
		}); ok {
			relay = relayer.RelayApprovalRequest(sessionID, body.RequestID, body.Decision)
		} else {
			relay = map[string]any{"ok": false, "detail": "provider does not support request-scoped approval"}
		}
	} else {
		relay = p.RelayApproval(sessionID, body.Decision)
	}
	code := http.StatusOK
	if !truthy(relay["ok"], false) {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, map[string]any{"ok": truthy(relay["ok"], false), "decision": body.Decision, "detail": stringAny(relay["detail"])})
}

func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	payload := buildPushPayload("test", "waiting_approval", "remote-coding push test")
	payload["device"] = s.cfg.DeviceID
	subs, _ := s.loadPushSubs()
	n := s.sendPushToAll(payload, true)
	writeJSON(w, http.StatusOK, map[string]any{"sent_to": n, "subscriptions": len(subs)})
}

func (s *Server) pushMonitorLoop() {
	ticker := time.NewTicker(pushMonitorEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.pushMonitorTick()
		case <-s.pushStop:
			return
		}
	}
}

func (s *Server) pushMonitorTick() {
	states := s.pushStates()
	seen := map[string]bool{}
	for _, row := range states {
		key := approvalIdentity(row.ProviderID, row.NativeSessionID)
		seen[key] = true
		sig := row.State + "|" + row.RequestID
		s.pushMu.Lock()
		prev := s.pushLast[key]
		s.pushLast[key] = sig
		s.pushMu.Unlock()
		if pushActionableStates[row.State] && prev != sig {
			s.pushSend(row)
		}
	}
	s.pushMu.Lock()
	for key := range s.pushLast {
		if !seen[key] {
			delete(s.pushLast, key)
		}
	}
	s.pushMu.Unlock()
}

func (s *Server) pushStates() []pushState {
	rows := s.pendingApprovalEntries("")
	out := make([]pushState, 0, len(rows))
	for _, row := range rows {
		state := firstNonEmpty(stringAny(row["interaction_state"]), "waiting_approval")
		ctx := strings.TrimSpace(firstNonEmpty(stringAny(row["summary"]), firstNonEmpty(stringAny(row["question"]), stringAny(row["header"]))))
		out = append(out, pushState{
			ProviderID: stringAny(row["provider_id"]), SessionID: stringAny(row["session_id"]),
			NativeSessionID: stringAny(row["native_session_id"]), RequestID: stringAny(row["request_id"]),
			State: state, Context: ctx,
		})
	}
	return out
}

func (s *Server) pushSend(row pushState) {
	if s.presenceFresh(row.ProviderID, row.NativeSessionID) {
		return
	}
	payload := buildProviderPushPayload(row.ProviderID, row.SessionID, row.NativeSessionID, row.RequestID, row.State, row.Context)
	payload["device"] = s.cfg.DeviceID
	s.pushSender(payload)
}

func (s *Server) presenceFresh(providerID string, sessionID string) bool {
	s.presenceMu.Lock()
	ts := s.presence[approvalIdentity(providerID, sessionID)]
	if ts.IsZero() {
		ts = s.presence[approvalIdentity("", sessionID)]
	}
	if ts.IsZero() {
		ts = s.presence[sessionID]
	}
	s.presenceMu.Unlock()
	return !ts.IsZero() && time.Since(ts) < pushPresenceTTL
}

func buildPushPayload(sessionID string, state string, ctx string) map[string]any {
	return buildProviderPushPayload("", sessionID, sessionID, "", state, ctx)
}

func buildProviderPushPayload(providerID string, sessionID string, nativeSessionID string, requestID string, state string, ctx string) map[string]any {
	title := "🔔 待批准"
	body := truncatePushBody(ctx)
	kind := "approval"
	if state == "waiting_input" {
		title = "🔔 待回答"
		kind = "input"
		if body == "" {
			body = "Claude 有一个问题等你回答"
		}
	} else if body == "" {
		body = "Claude 有一个操作等你批准"
	}
	q := url.Values{}
	q.Set("focus", nativeSessionID)
	if providerID != "" {
		q.Set("provider", providerID)
	}
	return map[string]any{
		"title":          title,
		"body":           body,
		"kind":           kind,
		"tag":            "remotecoding-" + providerID + "-" + nativeSessionID,
		"url":            "/s/remotecoding/?" + q.Encode(),
		"session":        sessionID,
		"native_session": nativeSessionID,
		"provider":       providerID,
		"request_id":     requestID,
	}
}

func truncatePushBody(v string) string {
	v = strings.TrimSpace(v)
	if len([]rune(v)) <= pushBodyLimit {
		return v
	}
	runes := []rune(v)
	return string(runes[:pushBodyLimit-1]) + "…"
}

func (s *Server) sendPushToAll(payload map[string]any, async bool) int {
	subs, err := s.loadPushSubs()
	if err != nil || len(subs) == 0 {
		if err != nil {
			log.Printf("push: load subscriptions failed: %v", err)
		}
		return 0
	}
	keys, err := s.loadOrCreateVAPID()
	if err != nil {
		log.Printf("push: load VAPID failed: %v", err)
		return 0
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("push: marshal payload failed: %v", err)
		return 0
	}
	for _, sub := range subs {
		sub := sub
		if async {
			go s.sendPushOne(sub, keys, data)
		} else {
			s.sendPushOne(sub, keys, data)
		}
	}
	return len(subs)
}

func (s *Server) sendPushOne(sub pushSub, vapid map[string]string, payload []byte) {
	req, err := newWebPushRequest(sub, vapid, payload)
	if err != nil {
		log.Printf("push: build request failed: %v", err)
		_ = s.bumpPushFail(sub.Endpoint)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if proxy := s.pushProxy(); proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			log.Printf("push: bad proxy %q: %v", proxy, err)
		} else {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = http.ProxyURL(u)
			client.Transport = tr
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("push: send failed: %v", err)
		_ = s.bumpPushFail(sub.Endpoint)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		_ = s.removePushSub(sub.Endpoint)
		return
	}
	if resp.StatusCode > http.StatusAccepted {
		log.Printf("push: send failed status=%d", resp.StatusCode)
		_ = s.bumpPushFail(sub.Endpoint)
	}
}

func (s *Server) pushProxy() string {
	if s.cfg.PushProxy != "" {
		return s.cfg.PushProxy
	}
	return os.Getenv("RC_PUSH_PROXY")
}

func (s *Server) pushDir() string {
	dataDir := s.store.DataDir()
	if filepath.Base(dataDir) == "data" {
		return filepath.Join(filepath.Dir(dataDir), "push")
	}
	return filepath.Join(dataDir, "push")
}

func (s *Server) pushSubsPath() string {
	return filepath.Join(s.pushDir(), "subscriptions.json")
}

func (s *Server) legacyPushSubsPath() string {
	return filepath.Join(s.store.DataDir(), "push_subscriptions.json")
}

func (s *Server) vapidPath() string {
	return filepath.Join(s.pushDir(), "vapid.json")
}

func (s *Server) legacyVAPIDPath() string {
	return filepath.Join(s.store.DataDir(), "push_vapid.json")
}

func (s *Server) loadPushSubs() ([]pushSub, error) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	for _, p := range []string{s.pushSubsPath(), s.legacyPushSubsPath()} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return []pushSub{}, nil
		}
		var subs []pushSub
		if err := json.Unmarshal(b, &subs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		return subs, nil
	}
	return []pushSub{}, nil
}

func (s *Server) savePushSubs(subs []pushSub) error {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	return writeJSON0600(s.pushSubsPath(), subs)
}

func (s *Server) removePushSub(endpoint string) error {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	subs, err := s.loadPushSubsNoLock()
	if err != nil {
		return err
	}
	kept := subs[:0]
	for _, sub := range subs {
		if sub.Endpoint != endpoint {
			kept = append(kept, sub)
		}
	}
	return writeJSON0600(s.pushSubsPath(), kept)
}

func (s *Server) bumpPushFail(endpoint string) error {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	subs, err := s.loadPushSubsNoLock()
	if err != nil {
		return err
	}
	changed := false
	kept := subs[:0]
	for _, sub := range subs {
		if sub.Endpoint == endpoint {
			sub.FailCount++
			changed = true
			if sub.FailCount >= pushFailThreshold {
				continue
			}
		}
		kept = append(kept, sub)
	}
	if !changed {
		return nil
	}
	return writeJSON0600(s.pushSubsPath(), kept)
}

func (s *Server) loadPushSubsNoLock() ([]pushSub, error) {
	for _, p := range []string{s.pushSubsPath(), s.legacyPushSubsPath()} {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return []pushSub{}, nil
		}
		var subs []pushSub
		if err := json.Unmarshal(b, &subs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		return subs, nil
	}
	return []pushSub{}, nil
}

func (s *Server) loadOrCreateVAPID() (map[string]string, error) {
	if s.cfg.PushVAPID != nil && s.cfg.PushVAPID["private_pem"] != "" && s.cfg.PushVAPID["public_b64"] != "" {
		return s.cfg.PushVAPID, nil
	}
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	for _, p := range []string{s.vapidPath(), s.legacyVAPIDPath()} {
		keys, ok, err := readVAPIDFile(p)
		if err != nil {
			return nil, err
		}
		if ok {
			if p != s.vapidPath() {
				_ = writeJSON0600(s.vapidPath(), keys)
			}
			return keys, nil
		}
	}
	keys, err := generateVAPIDKeys()
	if err != nil {
		return nil, err
	}
	if err := writeJSON0600(s.vapidPath(), keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func readVAPIDFile(path string) (map[string]string, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var keys map[string]string
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if keys["private_pem"] == "" || keys["public_b64"] == "" {
		return nil, false, fmt.Errorf("VAPID file %s is missing required keys", path)
	}
	return keys, true, nil
}

func generateVAPIDKeys() (map[string]string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	pub := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	return map[string]string{"private_pem": privatePEM, "public_b64": b64URLNoPad(pub)}, nil
}

func writeJSON0600(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseVAPIDPrivateKey(privatePEM string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("VAPID private key PEM is invalid")
	}
	if keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := keyAny.(*ecdsa.PrivateKey); ok && key.Curve == elliptic.P256() {
			return key, nil
		}
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil && key.Curve == elliptic.P256() {
		return key, nil
	}
	return nil, errors.New("VAPID private key is not a P-256 EC key")
}

func b64URLNoPad(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func endpointHash(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])[:12]
}
