package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/provider"
)

const computerUseAutomationTurnPrefix = "trusted-ui-"

// installComputerUseReadinessHandler exposes only the helper's capability and
// safety state. It deliberately drops its audit ring and public verifying key,
// and the provider may use this snapshot only for status presentation.
func (s *Server) installComputerUseReadinessHandler(host provider.ComputerUseReadinessHost) {
	host.SetComputerUseReadinessHandler(func(ctx context.Context) provider.ComputerUseReadiness {
		status := provider.ComputerUseReadiness{
			Enabled:          s.cfg.ComputerUse.Enabled,
			LockedUseEnabled: s.cfg.ComputerUse.LockedUse.Enabled,
		}
		if ctx == nil || ctx.Err() != nil {
			status.Detail = "computer-use readiness check was cancelled"
			return status
		}
		if s.computerUseCtl == nil {
			status.Detail = "computer use is not configured on this device"
			return status
		}
		raw := s.computerUseCtl.StatusContext(ctx)
		if value, ok := raw["enabled"].(bool); ok {
			status.Enabled = value
		}
		if value, ok := raw["available"].(bool); ok {
			status.Available = value
		}
		status.Detail = strings.TrimSpace(stringAny(raw["detail"]))
		lockedUse, _ := raw["locked_use"].(map[string]any)
		if lockedUse == nil {
			return status
		}
		if value, ok := lockedUse["enabled"].(bool); ok {
			status.LockedUseEnabled = value
		}
		status.LockedUseArmed = truthy(lockedUse["armed"], false)
		status.LockedUseActive = truthy(lockedUse["active"], false)
		status.LockedUseSuppressed = truthy(lockedUse["suppressed_until_manual_unlock"], false)
		status.LockedUseQuarantined = truthy(lockedUse["quarantined"], false)
		status.RequiresManualRecovery = truthy(lockedUse["requires_manual_recovery"], false)
		status.Stopping = truthy(lockedUse["stopping"], false)
		if status.Detail == "" {
			status.Detail = strings.TrimSpace(stringAny(lockedUse["error"]))
		}
		return status
	})
}

// installComputerUseAutomationHandler gives a trusted provider adapter one
// callback-scoped desktop transaction. Unlike model tool hosting, the server
// creates the operation turn itself and fixes both provider and logical
// session outside every tool request.
func (s *Server) installComputerUseAutomationHandler(
	providerID string, host provider.ComputerUseAutomationHost,
) {
	host.SetComputerUseAutomationHandler(func(
		ctx context.Context, sessionID string, callback provider.ComputerUseAutomationCallback,
	) error {
		return s.runComputerUseAutomation(ctx, providerID, sessionID, callback)
	})
}

func (s *Server) runComputerUseAutomation(
	ctx context.Context,
	providerID string,
	sessionID string,
	callback provider.ComputerUseAutomationCallback,
) (resultErr error) {
	if ctx == nil {
		return errors.New("computer-use automation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if callback == nil {
		return errors.New("computer-use automation requires a callback")
	}
	if s.computerUseCtl == nil {
		return computeruse.ErrNotEnabled
	}

	providerID = canonicalProviderID(providerID)
	logicalSessionID, err := s.storedComputerUseAutomationSession(providerID, sessionID)
	if err != nil {
		return err
	}
	ref, err := s.beginComputerUseAutomationLease(providerID, logicalSessionID)
	if err != nil {
		return err
	}

	var live atomic.Bool
	live.Store(true)
	var calls sync.RWMutex

	// Cleanup is synchronous and does not inherit a cancelled callback context:
	// once the helper may have opened a window, relock is an obligation rather
	// than optional follow-up work. The defer also runs while unwinding a panic.
	defer func() {
		// Revoke callback liveness before waiting for already-started calls. A
		// retained handler that races callback return cannot enter the helper in
		// the gap before the lease cleanup acquires computerUseMu.
		live.Store(false)
		if s.computerUseAutomationRevokedHook != nil {
			s.computerUseAutomationRevokedHook()
		}
		calls.Lock()
		calls.Unlock()
		cleanupErr := s.endComputerUseAutomationLease(ref, "trusted UI automation transaction ended")
		if recovered := recover(); recovered != nil {
			panic(recovered)
		}
		if cleanupErr != nil {
			cleanupErr = errors.Join(
				provider.ErrComputerUseAutomationCleanup,
				fmt.Errorf("computer-use automation could not confirm cleanup: %w", cleanupErr),
			)
		}
		resultErr = errors.Join(resultErr, cleanupErr)
	}()

	scopedHandler := func(
		callCtx context.Context, request provider.ComputerUseToolRequest,
	) (provider.ComputerUseToolResult, error) {
		if !live.Load() {
			return provider.ComputerUseToolResult{}, computeruse.ErrTurnNotActive
		}
		calls.RLock()
		defer calls.RUnlock()
		if !live.Load() {
			return provider.ComputerUseToolResult{}, computeruse.ErrTurnNotActive
		}
		if err := ctx.Err(); err != nil {
			return provider.ComputerUseToolResult{}, err
		}
		if callCtx == nil {
			return provider.ComputerUseToolResult{}, errors.New("computer-use automation tool requires a context")
		}
		if err := callCtx.Err(); err != nil {
			return provider.ComputerUseToolResult{}, err
		}
		request.ProviderID = ref.ProviderID
		request.SessionID = ref.Target
		request.ThreadID = ""
		request.TurnID = ref.TurnID
		request.CallID = ""
		return s.handleComputerUseTool(callCtx, request)
	}

	return callback(ctx, scopedHandler)
}

// storedComputerUseAutomationSession intentionally accepts only the stored
// logical id, not a native transcript/thread alias or a provider runtime row.
// A trusted adapter must first bind the UI operation to AgentHalo's durable
// provider-scoped session record.
func (s *Server) storedComputerUseAutomationSession(providerID, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if err := rejectUnsafeSessionID(sessionID); err != nil {
		return "", err
	}
	if _, resolved, ok := s.getProvider(providerID); !ok || resolved != providerID {
		return "", errors.New("unknown provider_id")
	}
	record, ok, err := s.findSessionForProviderAny(providerID, sessionID)
	if err != nil {
		return "", err
	}
	if !ok || recordString(record, "session_id") != sessionID {
		return "", errors.New("computer-use automation requires a stored logical session")
	}
	return sessionID, nil
}

func (s *Server) beginComputerUseAutomationLease(
	providerID, logicalSessionID string,
) (computerUseLeaseRef, error) {
	for attempts := 0; attempts < 3; attempts++ {
		turnID, err := newComputerUseAutomationTurnID()
		if err != nil {
			return computerUseLeaseRef{}, err
		}
		ref, err := s.computerUseLeaseReference(providerID, logicalSessionID, turnID)
		if err != nil {
			return computerUseLeaseRef{}, err
		}
		key := computerUseLeaseKey(ref.ProviderID, ref.Target)
		identity := computerUseIdentity(ref.ProviderID, ref.Target, ref.TurnID)

		s.computerUseMu.Lock()
		if current, ok := s.computerUseTurns[key]; ok && current.Active {
			s.computerUseMu.Unlock()
			return computerUseLeaseRef{}, computeruse.ErrWindowBusy
		}
		if _, collision := s.computerUseEnded[identity]; collision {
			s.computerUseMu.Unlock()
			continue
		}
		s.computerUseTurns[key] = computerUseTurnLease{
			ProviderID: ref.ProviderID,
			Target:     ref.Target,
			TurnID:     ref.TurnID,
			OwnerID:    ref.OwnerID,
			Active:     true,
		}
		s.computerUseMu.Unlock()
		return ref, nil
	}
	return computerUseLeaseRef{}, errors.New("could not allocate a unique computer-use automation turn")
}

func (s *Server) endComputerUseAutomationLease(ref computerUseLeaseRef, reason string) error {
	key := computerUseLeaseKey(ref.ProviderID, ref.Target)
	s.computerUseMu.Lock()
	defer s.computerUseMu.Unlock()

	s.computerUseEnded[computerUseIdentity(ref.ProviderID, ref.Target, ref.TurnID)] = struct{}{}
	if current, ok := s.computerUseTurns[key]; ok &&
		current.TurnID == ref.TurnID && current.OwnerID == ref.OwnerID {
		current.Active = false
		s.computerUseTurns[key] = current
	}
	return s.closeComputerUseOwnerLocked(ref.OwnerID, reason)
}

func newComputerUseAutomationTurnID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate computer-use automation turn: %w", err)
	}
	return computerUseAutomationTurnPrefix + hex.EncodeToString(random[:]), nil
}
