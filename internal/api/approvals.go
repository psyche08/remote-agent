package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/psyche08/remote-agent/internal/state"
)

type pendingSessionCandidate struct {
	providerID string
	sessionID  string
	transcript string
	title      string
	updatedAt  string
	source     string
}

// pendingApprovals is the provider-independent approval inbox. Unlike
// /status, it is not tied to the process-global active session: every stored
// or currently-live native session is checked with provider-scoped identity.
func (s *Server) pendingApprovals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows := s.pendingApprovalEntries(strings.TrimSpace(r.URL.Query().Get("provider_id")))
	writeJSON(w, http.StatusOK, map[string]any{"approvals": rows})
}

func (s *Server) pendingApprovalEntries(filterProvider string) []map[string]any {
	candidates := s.pendingSessionCandidates(filterProvider)
	out := make([]map[string]any, 0)
	for _, c := range candidates {
		p := s.registry[c.providerID]
		if p == nil {
			continue
		}
		if binder, ok := p.(interface{ BindTranscript(string, string) }); ok {
			binder.BindTranscript(c.sessionID, c.transcript)
			binder.BindTranscript(c.transcript, c.transcript)
		}
		var request map[string]any
		if approvals, ok := p.(interface{ ApprovalRequest(string) map[string]any }); ok {
			request = approvals.ApprovalRequest(c.sessionID)
			if request == nil && c.sessionID != c.transcript {
				request = approvals.ApprovalRequest(c.transcript)
			}
		}
		if request == nil {
			if questions, ok := p.(interface{ PendingQuestion(string) map[string]any }); ok {
				request = questions.PendingQuestion(c.sessionID)
				if request == nil && c.sessionID != c.transcript {
					request = questions.PendingQuestion(c.transcript)
				}
			}
		}
		if request == nil {
			continue
		}
		row := map[string]any{}
		for k, v := range request {
			row[k] = v
		}
		row["provider_id"] = c.providerID
		row["session_id"] = c.sessionID
		row["native_session_id"] = c.transcript
		row["transcript_id"] = c.transcript
		row["approval_key"] = approvalIdentity(c.providerID, c.transcript)
		row["title"] = firstNonEmpty(c.title, c.transcript)
		row["updated_at"] = c.updatedAt
		if stringAny(row["source"]) == "" {
			row["source"] = c.source
		}
		row["interaction_state"] = pendingInteractionState(row)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return stringAny(out[i]["updated_at"]) > stringAny(out[j]["updated_at"])
	})
	return out
}

func (s *Server) pendingSessionCandidates(filterProvider string) map[string]pendingSessionCandidate {
	candidates := map[string]pendingSessionCandidate{}
	add := func(c pendingSessionCandidate) {
		if c.providerID == "" || c.sessionID == "" {
			return
		}
		if filterProvider != "" && c.providerID != filterProvider {
			return
		}
		if c.transcript == "" {
			c.transcript = c.sessionID
		}
		key := c.providerID + "\x00" + c.transcript
		if prev, ok := candidates[key]; ok {
			if strings.HasPrefix(prev.sessionID, "r-") || (prev.sessionID != prev.transcript && prev.sessionID != "") {
				c.sessionID = prev.sessionID
			}
			if prev.title != "" {
				c.title = prev.title
			}
			if prev.updatedAt > c.updatedAt {
				c.updatedAt = prev.updatedAt
			}
			if c.source == "" {
				c.source = prev.source
			}
		}
		candidates[key] = c
	}

	records, _ := s.store.Sessions()
	// Stale historical sessions are deliberately not scanned on every push
	// tick. Claude transcript recovery can touch large JSONL files, so the
	// candidate set is limited to live runtime rows, provider-reported pending
	// request IDs, the active session, and records already marked waiting.
	for _, rec := range records {
		if stateValue := recordString(rec, "state"); stateValue == "waiting_approval" || stateValue == "waiting_input" {
			add(pendingSessionCandidate{
				providerID: recordString(rec, "provider_id"), sessionID: recordString(rec, "session_id"),
				transcript: firstNonEmpty(recordString(rec, "transcript_id"), recordString(rec, "native_session_id")),
				title:      recordString(rec, "title"), updatedAt: recordString(rec, "updated_at"), source: "stored",
			})
		}
	}
	s.mu.Lock()
	activeProvider := s.activeProvider
	activeSession := ""
	if s.activeSessionID != nil {
		activeSession = *s.activeSessionID
	}
	s.mu.Unlock()
	if activeProvider != "" && activeSession != "" {
		transcript := activeSession
		if rec := matchingStoredSession(records, activeProvider, activeSession, ""); rec != nil {
			transcript = firstNonEmpty(recordString(rec, "transcript_id"), recordString(rec, "native_session_id"))
		}
		add(pendingSessionCandidate{providerID: activeProvider, sessionID: activeSession, transcript: transcript, source: "active"})
	}
	for _, pid := range s.registry.IDs() {
		if filterProvider != "" && pid != filterProvider {
			continue
		}
		p := s.registry[pid]
		if runtime, ok := p.(interface{ RuntimeSessions() []map[string]any }); ok {
			for _, row := range runtime.RuntimeSessions() {
				runtimeState := firstNonEmpty(stringAny(row["state"]), stringAny(row["status"]))
				if (runtimeState == "idle" || runtimeState == "completed") && !truthy(row["running"], false) {
					// Explicitly idle connected aliases remain available for a
					// future prompt, but cannot currently be blocking on a native
					// permission. Provider-reported pending IDs below still win.
					continue
				}
				transcript := firstNonEmpty(stringAny(row["transcript_id"]), firstNonEmpty(stringAny(row["cli_session_id"]), stringAny(row["native_session_id"])))
				logical := stringAny(row["session_id"])
				title := stringAny(row["title"])
				updatedAt := stringAny(row["updated_at"])
				if rec := matchingStoredSession(records, pid, logical, transcript); rec != nil {
					logical = recordString(rec, "session_id")
					title = firstNonEmpty(recordString(rec, "title"), title)
					updatedAt = firstNonEmpty(updatedAt, recordString(rec, "updated_at"))
				}
				add(pendingSessionCandidate{
					providerID: pid, sessionID: firstNonEmpty(logical, transcript), transcript: transcript,
					title: title, updatedAt: updatedAt, source: stringAny(row["source"]),
				})
			}
		}
		if pending, ok := p.(interface{ PendingApprovalSessionIDs() []string }); ok {
			for _, id := range pending.PendingApprovalSessionIDs() {
				c := pendingSessionCandidate{providerID: pid, sessionID: id, transcript: id, source: "pending"}
				if rec := matchingStoredSession(records, pid, id, id); rec != nil {
					c.sessionID = recordString(rec, "session_id")
					c.transcript = firstNonEmpty(recordString(rec, "transcript_id"), recordString(rec, "native_session_id"))
					c.title = recordString(rec, "title")
					c.updatedAt = recordString(rec, "updated_at")
				}
				add(c)
			}
		}
	}

	return candidates
}

func matchingStoredSession(records []state.Record, providerID string, sessionID string, transcriptID string) state.Record {
	for _, rec := range records {
		if !sameProviderID(recordString(rec, "provider_id"), providerID) {
			continue
		}
		if sessionID != "" && recordString(rec, "session_id") == sessionID {
			return rec
		}
		if transcriptID != "" && (recordString(rec, "transcript_id") == transcriptID || recordString(rec, "native_session_id") == transcriptID) {
			return rec
		}
	}
	return nil
}

func approvalIdentity(providerID string, sessionID string) string {
	return providerID + "|" + sessionID
}

func pendingInteractionState(request map[string]any) string {
	if request == nil {
		return ""
	}
	if stringAny(request["type"]) == "question" || !truthy(request["actionable"], true) {
		return "waiting_input"
	}
	return "waiting_approval"
}
