package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/psyche08/remote-agent/internal/provider"
)

const uploadMaxBytes = 25 * 1024 * 1024

var (
	attachmentIDRE = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	assetIDRE      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type uploadMetadata struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	File      string `json:"file"`
}

func uploadScope(providerID string, sessionID string) string {
	sum := sha256.Sum256([]byte(providerID + "\x00" + sessionID))
	return hex.EncodeToString(sum[:16])
}

func cleanUploadName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." {
		name = "attachment"
	}
	if len(name) > 160 {
		name = name[:160]
	}
	return name
}

func (s *Server) uploadDir(providerID string, sessionID string) string {
	return filepath.Join(s.store.DataDir(), "uploads", uploadScope(providerID, sessionID))
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes+1024*1024)
	if err := r.ParseMultipartForm(uploadMaxBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid or oversized upload")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	providerID, sessionID := r.FormValue("provider_id"), r.FormValue("session_id")
	if sessionID == "" || rejectUnsafeSessionID(sessionID) != nil {
		writeError(w, http.StatusBadRequest, "valid session_id is required")
		return
	}
	p, resolvedProviderID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	_ = p
	session, found, err := s.findSessionForProviderAny(resolvedProviderID, sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found || !sameProviderID(recordString(session, "provider_id"), resolvedProviderID) {
		writeError(w, http.StatusNotFound, "unknown session_id")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, uploadMaxBytes+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read upload failed")
		return
	}
	if len(data) == 0 || len(data) > uploadMaxBytes {
		writeError(w, http.StatusBadRequest, "file must be between 1 byte and 25 MB")
		return
	}
	name := cleanUploadName(header.Filename)
	mime := http.DetectContentType(data)
	if declared := strings.TrimSpace(header.Header.Get("Content-Type")); strings.HasPrefix(declared, "image/") && strings.HasPrefix(mime, "image/") {
		mime = declared
	}
	id := newID()
	dir := s.uploadDir(resolvedProviderID, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "create upload directory failed")
		return
	}
	fileName := id + "-" + name
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "store upload failed")
		return
	}
	meta := uploadMetadata{ID: id, Provider: resolvedProviderID, SessionID: sessionID, Name: name, MediaType: mime, Size: int64(len(data)), File: fileName}
	encoded, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), encoded, 0o600); err != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, "store upload metadata failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "attachment": map[string]any{"id": id, "name": name, "media_type": mime, "size": len(data)},
	})
}

func (s *Server) loadAttachments(providerID string, sessionID string, ids []string) ([]provider.Attachment, error) {
	if len(ids) > 12 {
		return nil, errors.New("at most 12 attachments are allowed")
	}
	dir := s.uploadDir(providerID, sessionID)
	out := make([]provider.Attachment, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !attachmentIDRE.MatchString(id) || seen[id] {
			return nil, errors.New("invalid attachment id")
		}
		seen[id] = true
		encoded, err := os.ReadFile(filepath.Join(dir, id+".json"))
		if err != nil {
			return nil, errors.New("attachment is unavailable")
		}
		var meta uploadMetadata
		if json.Unmarshal(encoded, &meta) != nil || meta.ID != id || meta.Provider != providerID || meta.SessionID != sessionID {
			return nil, errors.New("attachment does not belong to this session")
		}
		path := filepath.Join(dir, filepath.Base(meta.File))
		st, err := os.Stat(path)
		if err != nil || st.IsDir() || st.Size() != meta.Size {
			return nil, errors.New("attachment is unavailable")
		}
		out = append(out, provider.Attachment{ID: id, Name: meta.Name, Path: path, MediaType: meta.MediaType, Size: meta.Size})
	}
	return out, nil
}

func (s *Server) sessionAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providerID, sessionID, assetID := r.URL.Query().Get("provider_id"), r.URL.Query().Get("session_id"), r.URL.Query().Get("asset_id")
	if sessionID == "" || rejectUnsafeSessionID(sessionID) != nil || !assetIDRE.MatchString(assetID) {
		writeError(w, http.StatusBadRequest, "valid session_id and asset_id are required")
		return
	}
	p, providerID, ok := s.getProvider(providerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider_id")
		return
	}
	reader, ok := p.(provider.SessionAssetReader)
	if !ok {
		writeError(w, http.StatusNotFound, "session asset is unavailable")
		return
	}
	s.bindProviderTranscript(p, sessionID)
	asset, found, err := reader.ReadSessionAsset(sessionID, assetID)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "session asset not found")
		return
	}
	if !strings.HasPrefix(asset.MediaType, "image/") {
		writeError(w, http.StatusUnsupportedMediaType, "session asset is not an image")
		return
	}
	w.Header().Set("Content-Type", asset.MediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(asset.Data)))
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset.Data)
}
