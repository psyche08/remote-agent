package turnstatehook

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const interactionAttemptDir = ".attempts"

var (
	ErrInteractionAlreadyAttempted = errors.New("Claude interaction was already attempted")
	ErrInteractionAttemptConflict  = errors.New("Claude interaction attempt conflicts with its durable ledger")
)

// InteractionCandidateState is the provider-facing, read-only view of an
// observer candidate. Only pending is actionable. Attempted means a native UI
// mutation may already have happened and its postcondition is unknown;
// resolved means the verified tombstone may be hidden from discovery.
type InteractionCandidateState string

const (
	InteractionCandidatePending   InteractionCandidateState = "pending"
	InteractionCandidateAttempted InteractionCandidateState = "attempted"
	InteractionCandidateResolved  InteractionCandidateState = "resolved"
)

// InteractionAttempt is a durable exact-once tombstone independent from the
// observer candidate file. ObserveInteraction may rewrite its latest snapshot,
// but can never remove or reset this attempted/resolved state.
type InteractionAttempt struct {
	RequestID      string     `json:"request_id"`
	OperationID    string     `json:"operation_id"`
	IdentityDigest string     `json:"identity_digest"`
	LogicalSession string     `json:"logical_session"`
	Kind           string     `json:"kind"`
	DecisionDigest string     `json:"decision_digest"`
	State          string     `json:"state"`
	AttemptedAt    time.Time  `json:"attempted_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
}

// BeginInteractionAttempt atomically creates the attempted tombstone before a
// resume deep-link, permission press, or answer mutation. O_EXCL is deliberate:
// even a crash during the first write leaves a fail-closed inode that no retry
// may interpret as pending.
func BeginInteractionAttempt(
	dir, requestID, identityDigest, logicalSession, kind, decisionDigest string,
) (InteractionAttempt, error) {
	requestID = strings.TrimSpace(requestID)
	logicalSession = strings.TrimSpace(logicalSession)
	kind = strings.TrimSpace(kind)
	identityDigest = strings.ToLower(strings.TrimSpace(identityDigest))
	decisionDigest = strings.ToLower(strings.TrimSpace(decisionDigest))
	if requestID == "" || logicalSession == "" {
		return InteractionAttempt{}, errors.New("interaction attempt requires request and logical session ids")
	}
	if err := validateAttemptDigest(identityDigest); err != nil {
		return InteractionAttempt{}, fmt.Errorf("invalid interaction identity digest: %w", err)
	}
	if err := validateAttemptDigest(decisionDigest); err != nil {
		return InteractionAttempt{}, fmt.Errorf("invalid interaction decision digest: %w", err)
	}
	if kind != "permission" && kind != "question" && kind != "prompt" {
		return InteractionAttempt{}, errors.New("interaction attempt kind must be permission, question, or prompt")
	}
	operationID, err := newInteractionOperationID()
	if err != nil {
		return InteractionAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := InteractionAttempt{
		RequestID: requestID, OperationID: operationID,
		IdentityDigest: identityDigest, LogicalSession: logicalSession,
		Kind: kind, DecisionDigest: decisionDigest,
		State: "attempted", AttemptedAt: now,
	}
	attemptDir, err := secureInteractionAttemptDir(dir, true)
	if err != nil {
		return InteractionAttempt{}, err
	}
	path := filepath.Join(attemptDir, interactionFilename(requestID))
	b, err := json.Marshal(attempt)
	if err != nil {
		return InteractionAttempt{}, err
	}
	f, err := os.OpenFile(
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return classifyExistingInteractionAttempt(path, attempt)
		}
		return InteractionAttempt{}, err
	}
	writeErr := error(nil)
	if _, err := f.Write(b); err != nil {
		writeErr = err
	} else if err := f.Sync(); err != nil {
		writeErr = err
	}
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		// Never unlink a partial attempt: its existence is the fail-closed proof
		// that a side-effecting caller may have crossed the boundary.
		return InteractionAttempt{}, fmt.Errorf("persist interaction attempt: %w", writeErr)
	}
	if err := syncDirectoryForDurability(attemptDir); err != nil {
		// The inode is intentionally retained. A retry sees an attempted
		// tombstone and cannot cross the mutation boundary even though this
		// caller did not receive a durability acknowledgement.
		return InteractionAttempt{}, fmt.Errorf("persist interaction attempt directory entry: %w", err)
	}
	return attempt, nil
}

// ResolveInteractionAttempt records a verified native-UI postcondition. The
// tombstone remains durable indefinitely so an observer rewrite or restart
// cannot make the same request actionable again.
func ResolveInteractionAttempt(
	dir, requestID, operationID string,
) (InteractionAttempt, error) {
	attemptDir, err := secureInteractionAttemptDir(dir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InteractionAttempt{}, ErrInteractionNotFound
		}
		return InteractionAttempt{}, err
	}
	path := filepath.Join(attemptDir, interactionFilename(strings.TrimSpace(requestID)))
	attempt, err := readInteractionAttempt(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InteractionAttempt{}, ErrInteractionNotFound
		}
		return InteractionAttempt{}, err
	}
	if attempt.RequestID != strings.TrimSpace(requestID) ||
		attempt.OperationID != strings.TrimSpace(operationID) {
		return InteractionAttempt{}, ErrInteractionAttemptConflict
	}
	if attempt.State == "resolved" {
		return attempt, nil
	}
	if attempt.State != "attempted" {
		return InteractionAttempt{}, ErrInteractionAttemptConflict
	}
	now := time.Now().UTC()
	attempt.State = "resolved"
	attempt.ResolvedAt = &now
	if err := replaceInteractionAttempt(path, attempt); err != nil {
		return InteractionAttempt{}, err
	}
	return attempt, nil
}

// ReadInteractionAttempt reads attempted/resolved state without any TTL. A
// caller must treat both states as non-pending; only resolution affects audit
// interpretation, never exact-once admission.
func ReadInteractionAttempt(dir, requestID string) (InteractionAttempt, error) {
	attemptDir, err := secureInteractionAttemptDir(dir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InteractionAttempt{}, ErrInteractionNotFound
		}
		return InteractionAttempt{}, err
	}
	attempt, err := readInteractionAttempt(filepath.Join(
		attemptDir, interactionFilename(strings.TrimSpace(requestID)),
	))
	if errors.Is(err, os.ErrNotExist) {
		return InteractionAttempt{}, ErrInteractionNotFound
	}
	return attempt, err
}

// InteractionCandidateStateForRecord classifies an observer record without
// mutating either the candidate or its exact-once ledger. A rewritten
// candidate cannot become actionable after BeginInteractionAttempt: matching
// attempted/resolved ledgers remain non-pending, while an identity mismatch or
// damaged ledger is returned as a fail-closed error.
func InteractionCandidateStateForRecord(
	dir string, record InteractionRecord,
) (InteractionCandidateState, error) {
	attempt, err := ReadInteractionAttempt(dir, record.RequestID)
	if errors.Is(err, ErrInteractionNotFound) {
		return InteractionCandidatePending, nil
	}
	if err != nil {
		return "", err
	}
	identityDigest, err := InteractionIdentityDigest(record)
	if err != nil {
		return "", err
	}
	if attempt.IdentityDigest != identityDigest || attempt.RequestID != record.RequestID {
		return "", ErrInteractionAttemptConflict
	}
	switch attempt.State {
	case string(InteractionCandidateAttempted):
		return InteractionCandidateAttempted, nil
	case string(InteractionCandidateResolved):
		return InteractionCandidateResolved, nil
	default:
		return "", ErrInteractionAttemptConflict
	}
}

// InteractionIdentityDigest hashes the stable observer identity fields; it
// excludes timestamps so an idempotent observer rewrite yields the same proof.
func InteractionIdentityDigest(record InteractionRecord) (string, error) {
	return digestInteractionValue(struct {
		RequestID             string `json:"request_id"`
		SessionID             string `json:"session_id"`
		TranscriptPath        string `json:"transcript_path"`
		CWD                   string `json:"cwd"`
		Event                 string `json:"hook_event_name"`
		ToolName              string `json:"tool_name"`
		ToolUseID             string `json:"tool_use_id"`
		ToolInput             any    `json:"tool_input"`
		PermissionSuggestions any    `json:"permission_suggestions"`
	}{
		RequestID: record.RequestID, SessionID: record.SessionID,
		TranscriptPath: record.TranscriptPath, CWD: record.CWD,
		Event: record.HookEventName, ToolName: record.ToolName,
		ToolUseID: record.ToolUseID, ToolInput: record.ToolInput,
		PermissionSuggestions: record.PermissionSuggestions,
	})
}

func InteractionDecisionDigest(decision any) (string, error) {
	return digestInteractionValue(decision)
}

func digestInteractionValue(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func validateAttemptDigest(value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("digest must be a SHA-256 hex value")
	}
	return nil
}

func newInteractionOperationID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "interaction-op-" + hex.EncodeToString(random[:]), nil
}

func secureInteractionAttemptDir(dir string, create bool) (string, error) {
	if dir == "" {
		dir = defaultInteractionDir()
	}
	root, err := secureInteractionDir(dir, create)
	if err != nil {
		return "", err
	}
	attemptPath := filepath.Join(root, interactionAttemptDir)
	_, beforeErr := os.Lstat(attemptPath)
	attemptDir, err := secureInteractionDir(attemptPath, create)
	if err != nil {
		return "", err
	}
	if create && errors.Is(beforeErr, os.ErrNotExist) {
		// Persist the `.attempts` directory entry itself before relying on an
		// fsync of files inside it.
		if err := syncDirectoryForDurability(root); err != nil {
			return "", fmt.Errorf("persist interaction attempt directory: %w", err)
		}
	}
	return attemptDir, nil
}

func classifyExistingInteractionAttempt(
	path string, expected InteractionAttempt,
) (InteractionAttempt, error) {
	var existing InteractionAttempt
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		existing, err = readInteractionAttempt(path)
		if err == nil {
			break
		}
		// O_EXCL publishes the inode before the winner completes its small
		// fsync. Briefly wait for that writer; a crashed/partial inode remains a
		// permanent conflict after the bounded retry.
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		// A malformed/partial existing inode may mean the first process crashed
		// after O_EXCL. It is permanently conflicting, never retryable.
		return InteractionAttempt{}, errors.Join(ErrInteractionAttemptConflict, err)
	}
	if existing.RequestID == expected.RequestID &&
		existing.IdentityDigest == expected.IdentityDigest &&
		existing.LogicalSession == expected.LogicalSession &&
		existing.Kind == expected.Kind &&
		existing.DecisionDigest == expected.DecisionDigest {
		return existing, ErrInteractionAlreadyAttempted
	}
	return existing, ErrInteractionAttemptConflict
}

func readInteractionAttempt(path string) (InteractionAttempt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return InteractionAttempt{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return InteractionAttempt{}, fmt.Errorf("%w: attempt record must be a 0600 regular file", ErrUnsafeInteractionDir)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return InteractionAttempt{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInteractionBytes+1))
	if err != nil {
		return InteractionAttempt{}, err
	}
	if len(b) > maxInteractionBytes {
		return InteractionAttempt{}, errors.New("interaction attempt record is too large")
	}
	var attempt InteractionAttempt
	decoder := json.NewDecoder(bytes.NewReader(b))
	if err := decoder.Decode(&attempt); err != nil {
		return InteractionAttempt{}, err
	}
	if attempt.RequestID == "" || attempt.OperationID == "" ||
		(attempt.State != "attempted" && attempt.State != "resolved") ||
		attempt.AttemptedAt.IsZero() {
		return InteractionAttempt{}, errors.New("interaction attempt record is incomplete")
	}
	return attempt, nil
}

func replaceInteractionAttempt(path string, attempt InteractionAttempt) error {
	if info, err := os.Lstat(path); err != nil {
		return err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: attempt target is not a regular file", ErrUnsafeInteractionDir)
	}
	b, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".attempt-resolve-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncParentDirectoryForDurability(path); err != nil {
		return fmt.Errorf("persist resolved interaction attempt: %w", err)
	}
	keep = true
	return nil
}
