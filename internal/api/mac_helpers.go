package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dir := filepath.Join(filepath.Dir(s.store.DataDir()), "screenshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, "screenshot_"+time.Now().UTC().Format("20060102_150405")+".png")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "screencapture", "-x", path)
	if err := cmd.Run(); err != nil {
		writeError(w, http.StatusInternalServerError, "screenshot failed: "+err.Error())
		return
	}
	s.lastScreenshot = path
	s.lastShotAt = nowISO()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "url": "/last_screenshot", "captured_at": s.lastShotAt})
}

func (s *Server) lastScreenshotFile(w http.ResponseWriter, r *http.Request) {
	if s.lastScreenshot == "" {
		writeError(w, http.StatusNotFound, "no screenshot captured yet")
		return
	}
	http.ServeFile(w, r, s.lastScreenshot)
}

func (s *Server) clipboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pbpaste").Output()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "clipboard read failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": string(out), "read_at": nowISO()})
}

func (s *Server) copyReply(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"ok": false, "status": "not_supported",
		"detail": "current Go providers do not implement copy-button reply capture",
	})
}

func (s *Server) recover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
	}
	if r.Method == http.MethodPost {
		_ = jsonNewDecoder(r, &body)
	}
	p, providerID, ok := s.getProvider(body.ProviderID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	if rec, ok := p.(interface{ RecoverFromError() bool }); ok {
		ok := rec.RecoverFromError()
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "provider_id": providerID, "state": p.DetectState("")})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider_id": providerID, "state": p.DetectState(""), "detail": "no recovery action needed"})
}

func (s *Server) ocr(w http.ResponseWriter, r *http.Request) {
	if s.lastScreenshot == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "not_implemented", "engine": "apple_vision", "detail": "no screenshot available to OCR"})
		return
	}
	script := scriptPath("ocr_vision.swift")
	if script == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "not_implemented", "engine": "apple_vision", "detail": "swift/ocr_vision.swift not available"})
		return
	}
	if _, err := exec.LookPath("swift"); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "not_implemented", "engine": "apple_vision", "detail": "swift/ocr_vision.swift not available"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "swift", script, s.lastScreenshot).CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "engine": "apple_vision", "detail": string(out)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "engine": "apple_vision", "text": string(out)})
}

func jsonNewDecoder(r *http.Request, out any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	return jsonDecodeBody(r, out)
}

func jsonDecodeBody(r *http.Request, out any) error {
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(out)
	if err != nil && strings.Contains(err.Error(), "EOF") {
		return nil
	}
	return err
}

func scriptPath(name string) string {
	for _, p := range []string{filepath.Join("scripts", name), filepath.Join("remote-agent", "scripts", name)} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
