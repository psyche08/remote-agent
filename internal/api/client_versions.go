package api

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/psyche08/remote-agent/internal/buildinfo"
)

const (
	clientWebVersionHeader = "X-Remote-Coding-Web-Version"
	clientIDHeader         = "X-Remote-Coding-Client-Id"
	clientKindHeader       = "X-Remote-Coding-Client-Kind"
	clientVisibilityHeader = "X-Remote-Coding-Client-Visibility"
	clientVersionTTL       = 24 * time.Hour
	maxClientVersionRows   = 512
)

type clientVersionSeen struct {
	Key          string
	ClientID     string
	Kind         string
	WebVersion   string
	UserAgent    string
	RemoteHash   string
	FirstSeen    time.Time
	LastSeen     time.Time
	LastMethod   string
	LastPath     string
	LastProvider string
	LastSession  string
	Visibility   string
	Count        int
}

func (s *Server) captureClientVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.recordClientVersion(r)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recordClientVersion(r *http.Request) {
	webVersion := cleanClientHeader(firstNonEmpty(r.Header.Get(clientWebVersionHeader), r.URL.Query().Get("web_version")), 80)
	clientID := cleanClientHeader(firstNonEmpty(r.Header.Get(clientIDHeader), r.URL.Query().Get("client_id")), 120)
	if webVersion == "" && clientID == "" {
		return
	}

	key, publicID := clientVersionKey(r, clientID)
	now := time.Now()
	row := clientVersionSeen{
		Key:          key,
		ClientID:     publicID,
		Kind:         cleanClientHeader(firstNonEmpty(firstNonEmpty(r.Header.Get(clientKindHeader), r.URL.Query().Get("client_kind")), "web"), 40),
		WebVersion:   webVersion,
		UserAgent:    cleanClientHeader(r.UserAgent(), 180),
		RemoteHash:   shortClientHash(r.RemoteAddr),
		LastSeen:     now,
		LastMethod:   r.Method,
		LastPath:     cleanClientHeader(r.URL.Path, 160),
		LastProvider: cleanClientHeader(r.URL.Query().Get("provider_id"), 80),
		LastSession:  cleanClientHeader(r.URL.Query().Get("session_id"), 120),
		Visibility:   cleanClientHeader(r.Header.Get(clientVisibilityHeader), 40),
		Count:        1,
	}

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.pruneClientVersionsLocked(now)
	if prev := s.clients[key]; prev != nil {
		row.FirstSeen = prev.FirstSeen
		row.Count = prev.Count + 1
		if row.WebVersion == "" {
			row.WebVersion = prev.WebVersion
		}
		if row.Kind == "" {
			row.Kind = prev.Kind
		}
		if row.UserAgent == "" {
			row.UserAgent = prev.UserAgent
		}
		if row.RemoteHash == "" {
			row.RemoteHash = prev.RemoteHash
		}
		if row.LastProvider == "" {
			row.LastProvider = prev.LastProvider
		}
		if row.LastSession == "" {
			row.LastSession = prev.LastSession
		}
	} else {
		row.FirstSeen = now
	}
	s.clients[key] = &row
}

func (s *Server) clientVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	agent := buildinfo.Info()
	agentVersion := stringAny(agent["commit"], agent["version"])

	rows := []map[string]any{}
	s.clientMu.Lock()
	s.pruneClientVersionsLocked(now)
	for _, c := range s.clients {
		rows = append(rows, map[string]any{
			"client_id":     c.ClientID,
			"kind":          c.Kind,
			"web_version":   c.WebVersion,
			"agent_version": agentVersion,
			"first_seen":    c.FirstSeen.UTC().Format(time.RFC3339Nano),
			"last_seen":     c.LastSeen.UTC().Format(time.RFC3339Nano),
			"last_method":   c.LastMethod,
			"last_path":     c.LastPath,
			"provider_id":   c.LastProvider,
			"session_id":    c.LastSession,
			"visibility":    c.Visibility,
			"user_agent":    c.UserAgent,
			"remote_hash":   c.RemoteHash,
			"count":         c.Count,
		})
	}
	s.clientMu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		return stringAny(rows[i]["last_seen"]) > stringAny(rows[j]["last_seen"])
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"device_id":     s.cfg.DeviceID,
		"agent_version": agent,
		"count":         len(rows),
		"clients":       rows,
	})
}

func (s *Server) pruneClientVersionsLocked(now time.Time) {
	cutoff := now.Add(-clientVersionTTL)
	for key, row := range s.clients {
		if row.LastSeen.Before(cutoff) {
			delete(s.clients, key)
		}
	}
	if len(s.clients) <= maxClientVersionRows {
		return
	}
	rows := make([]*clientVersionSeen, 0, len(s.clients))
	for _, row := range s.clients {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastSeen.Before(rows[j].LastSeen) })
	for len(s.clients) > maxClientVersionRows && len(rows) > 0 {
		delete(s.clients, rows[0].Key)
		rows = rows[1:]
	}
}

func clientVersionKey(r *http.Request, explicit string) (string, string) {
	if explicit != "" {
		return "id:" + explicit, explicit
	}
	h := shortClientHash(r.RemoteAddr + "|" + r.UserAgent())
	return "anon:" + h, "anon-" + h
}

func shortClientHash(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sum := sha1.Sum([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}

func cleanClientHeader(v string, max int) string {
	v = strings.TrimSpace(v)
	if len(v) > max {
		v = v[:max]
	}
	return v
}
