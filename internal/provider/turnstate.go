package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type turnstateRecord struct {
	SessionID      string  `json:"session_id"`
	State          string  `json:"state"`
	TS             float64 `json:"ts"`
	Cwd            string  `json:"cwd"`
	TranscriptPath string  `json:"transcript_path"`
	Event          string  `json:"event"`
}

func readTurnstate(sessionID string, dir string) (*turnstateRecord, error) {
	path := filepath.Join(expandUser(dir), sessionID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec turnstateRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func listTurnstates(dir string) []*turnstateRecord {
	files, _ := filepath.Glob(filepath.Join(expandUser(dir), "*.json"))
	out := make([]*turnstateRecord, 0, len(files))
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec turnstateRecord
		if err := json.Unmarshal(b, &rec); err != nil || rec.SessionID == "" {
			continue
		}
		out = append(out, &rec)
	}
	return out
}

func turnstateRecordIdle(rec *turnstateRecord, staleAfter time.Duration) bool {
	if rec == nil {
		return true
	}
	if strings.ToLower(rec.State) != "running" {
		return true
	}
	if rec.TranscriptPath == "" {
		return false
	}
	st, err := os.Stat(expandUser(rec.TranscriptPath))
	if err != nil {
		return true
	}
	return time.Since(st.ModTime()) > staleAfter
}

func turnstateRecordUpdatedAt(rec *turnstateRecord) string {
	if rec == nil {
		return ""
	}
	if rec.TranscriptPath != "" {
		if mt := fileMTime(expandUser(rec.TranscriptPath)); !mt.IsZero() {
			return epochToISO(float64(mt.UnixNano()) / 1e9)
		}
	}
	if rec.TS > 0 {
		return epochToISO(rec.TS)
	}
	return ""
}

func turnstateIdle(sessionID string, dir string, staleAfter time.Duration) bool {
	rec, err := readTurnstate(sessionID, dir)
	if err != nil || rec == nil {
		return true
	}
	return turnstateRecordIdle(rec, staleAfter)
}

func waitTurnstateIdle(sessionID string, dir string, timeout time.Duration, staleAfter time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if turnstateIdle(sessionID, dir, staleAfter) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}
