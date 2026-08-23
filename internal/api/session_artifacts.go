package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

// persistSessionArtifacts writes only AgentHalo-owned derived files. Provider
// native transcripts remain read-only inputs and no native path crosses this
// API boundary.
func (s *Server) persistSessionArtifacts(
	p provider.Provider,
	providerID string,
	requestedSessionID string,
	messages []map[string]any,
) error {
	logicalID := requestedSessionID
	binding := state.SessionArtifactBinding{
		NativeSessionID: requestedSessionID,
		TranscriptID:    requestedSessionID,
		Source:          string(provider.Profile(p).AdapterKind),
		Surface:         string(provider.Profile(p).Surface),
	}
	record, found, err := s.findSessionForProviderAny(providerID, requestedSessionID)
	if err != nil {
		return fmt.Errorf("resolve session artifact binding: %w", err)
	}
	if found {
		if storedLogical := recordString(record, "session_id"); storedLogical != "" {
			logicalID = storedLogical
		}
		binding.NativeSessionID = recordString(record, "native_session_id")
		binding.TranscriptID = firstNonEmpty(recordString(record, "transcript_id"), binding.NativeSessionID)
		binding.Source = firstNonEmpty(recordString(record, "source"), binding.Source)
		binding.ControlRoute = firstNonEmpty(
			recordString(record, "claude_control_route"),
			firstNonEmpty(recordString(record, "codex_control_route"), recordString(record, "delivery_route")),
		)
	}
	identity := state.SessionArtifactIdentity{
		DeviceID:         s.cfg.DeviceID,
		ProviderID:       providerID,
		LogicalSessionID: logicalID,
	}
	derived := make([]state.SessionArtifactMessage, 0, len(messages))
	for _, message := range messages {
		timestamp := firstNonEmpty(stringAny(message["timestamp"]), stringAny(message["ts"]))
		if timestamp != "" {
			if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(timestamp)); err != nil {
				timestamp = ""
			}
		}
		derived = append(derived, state.SessionArtifactMessage{
			ID:        firstNonEmpty(stringAny(message["id"]), stringAny(message["message_id"])),
			Role:      stringAny(message["role"]),
			Kind:      stringAny(message["kind"]),
			Name:      stringAny(message["name"]),
			Text:      stringAny(message["text"]),
			Result:    stringAny(message["result"]),
			Timestamp: timestamp,
		})
	}
	if _, err := s.store.WriteSessionArtifacts(identity, binding, derived); err != nil {
		return fmt.Errorf("write session artifacts: %w", err)
	}
	return nil
}
