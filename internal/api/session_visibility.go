package api

import (
	"strings"

	"github.com/psyche08/remote-agent/internal/state"
)

const hiddenFromSessionListsKey = "hidden_from_lists"

type hiddenSessionIDProvider interface {
	HiddenSessionIDs() map[string]bool
}

func (s *Server) hiddenSessionIDs(providerID string) map[string]bool {
	providerID = canonicalProviderID(providerID)
	p := s.registry[providerID]
	if p == nil {
		return nil
	}
	lister, ok := p.(hiddenSessionIDProvider)
	if !ok {
		return nil
	}
	return lister.HiddenSessionIDs()
}

func sessionListIdentifiers(row map[string]any) []string {
	ids := make([]string, 0, 5)
	seen := map[string]bool{}
	for _, key := range []string{"session_id", "transcript_id", "native_session_id", "cli_session_id", "codex_thread_id"} {
		id := strings.TrimSpace(stringAny(row[key]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func sessionHiddenFromLists(row map[string]any, hiddenIDs map[string]bool) bool {
	if row == nil {
		return false
	}
	if truthy(row[hiddenFromSessionListsKey], false) || truthy(row["subagent"], false) {
		return true
	}
	for _, id := range sessionListIdentifiers(row) {
		if hiddenIDs[id] {
			return true
		}
	}
	return false
}

func visibleSessionRows(rows []map[string]any, hiddenIDs map[string]bool) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if !sessionHiddenFromLists(row, hiddenIDs) {
			out = append(out, row)
		}
	}
	return out
}

func sessionRecordMatchesID(rec state.Record, id string) bool {
	if id == "" {
		return true
	}
	for _, candidate := range sessionListIdentifiers(map[string]any(rec)) {
		if candidate == id {
			return true
		}
	}
	return false
}

// visibleStoredSessions keeps hidden child-agent records available through an
// exact-ID lookup so restored tabs and direct control remain functional.
func (s *Server) visibleStoredSessions(records []state.Record, providerID string, exactID string) []state.Record {
	providerID = canonicalProviderID(providerID)
	hiddenByProvider := map[string]map[string]bool{}
	out := make([]state.Record, 0, len(records))
	for _, rec := range records {
		pid := canonicalProviderID(recordString(rec, "provider_id"))
		if providerID != "" && pid != providerID {
			continue
		}
		if exactID != "" {
			if sessionRecordMatchesID(rec, exactID) {
				out = append(out, rec)
			}
			continue
		}
		if _, ok := hiddenByProvider[pid]; !ok {
			hiddenByProvider[pid] = s.hiddenSessionIDs(pid)
		}
		if sessionHiddenFromLists(map[string]any(rec), hiddenByProvider[pid]) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func (s *Server) hiddenStoredSessionKeys() map[string]bool {
	records, err := s.store.Sessions()
	if err != nil {
		return nil
	}
	hiddenByProvider := map[string]map[string]bool{}
	out := map[string]bool{}
	for _, rec := range records {
		pid := canonicalProviderID(recordString(rec, "provider_id"))
		if _, ok := hiddenByProvider[pid]; !ok {
			hiddenByProvider[pid] = s.hiddenSessionIDs(pid)
		}
		if !sessionHiddenFromLists(map[string]any(rec), hiddenByProvider[pid]) {
			continue
		}
		for _, id := range sessionListIdentifiers(map[string]any(rec)) {
			out[pid+"\x00"+id] = true
			out["\x00"+id] = true
		}
	}
	return out
}

func taskHiddenFromLists(task state.Record, hiddenSessionKeys map[string]bool) bool {
	if truthy(task[hiddenFromSessionListsKey], false) || truthy(task["subagent"], false) {
		return true
	}
	sessionID := recordString(task, "session_id")
	if sessionID == "" {
		return false
	}
	providerID := canonicalProviderID(recordString(task, "provider_id"))
	if providerID != "" {
		return hiddenSessionKeys[providerID+"\x00"+sessionID]
	}
	return hiddenSessionKeys["\x00"+sessionID]
}
