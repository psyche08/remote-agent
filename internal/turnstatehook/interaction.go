package turnstatehook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultInteractionTTL bounds how long an observed native Claude prompt can
	// be treated as current. Readers may ask for a shorter lifetime, but cannot
	// extend a record past the expiry written by the observer.
	DefaultInteractionTTL = 10 * time.Minute
	maxInteractionBytes   = 4 << 20
)

var (
	ErrInteractionNotFound  = errors.New("Claude interaction record not found")
	ErrInteractionExpired   = errors.New("Claude interaction record expired")
	ErrUnsafeInteractionDir = errors.New("unsafe Claude interaction directory")
)

// InteractionRecord is the allowlisted subset of a Claude hook payload that a
// trusted provider adapter needs to correlate native UI. It deliberately has
// no environment, process, credential, or arbitrary hook-payload field.
type InteractionRecord struct {
	RequestID             string    `json:"request_id"`
	SessionID             string    `json:"session_id"`
	TranscriptPath        string    `json:"transcript_path"`
	CWD                   string    `json:"cwd"`
	HookEventName         string    `json:"hook_event_name"`
	ToolName              string    `json:"tool_name"`
	ToolInput             any       `json:"tool_input"`
	ToolUseID             string    `json:"tool_use_id"`
	PermissionSuggestions any       `json:"permission_suggestions"`
	RecordedAt            time.Time `json:"recorded_at"`
	ExpiresAt             time.Time `json:"expires_at"`
}

type interactionHookPayload struct {
	SessionID             string `json:"session_id"`
	TranscriptPath        string `json:"transcript_path"`
	CWD                   string `json:"cwd"`
	HookEventName         string `json:"hook_event_name"`
	ToolName              string `json:"tool_name"`
	ToolInput             any    `json:"tool_input"`
	ToolUseID             string `json:"tool_use_id"`
	PermissionSuggestions any    `json:"permission_suggestions"`
}

// ObserveInteraction records a Claude PermissionRequest or AskUserQuestion
// notification without returning a hook decision. Callers own stdout: this
// function never writes to it and never interprets the request as permission.
func ObserveInteraction(input io.Reader, interactionDir string) (InteractionRecord, error) {
	var payload interactionHookPayload
	limited := io.LimitReader(input, maxInteractionBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return InteractionRecord{}, err
	}
	if len(raw) > maxInteractionBytes {
		return InteractionRecord{}, errors.New("Claude interaction payload is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return InteractionRecord{}, fmt.Errorf("decode Claude interaction hook: %w", err)
	}
	payload.SessionID = strings.TrimSpace(payload.SessionID)
	payload.TranscriptPath = strings.TrimSpace(payload.TranscriptPath)
	payload.CWD = strings.TrimSpace(payload.CWD)
	payload.HookEventName = strings.TrimSpace(payload.HookEventName)
	payload.ToolName = strings.TrimSpace(payload.ToolName)
	payload.ToolUseID = strings.TrimSpace(payload.ToolUseID)
	if payload.SessionID == "" {
		return InteractionRecord{}, errors.New("Claude interaction has no session_id")
	}
	if payload.HookEventName == "" {
		return InteractionRecord{}, errors.New("Claude interaction has no hook_event_name")
	}
	if payload.ToolName == "" {
		return InteractionRecord{}, errors.New("Claude interaction has no tool_name")
	}

	now := time.Now().UTC()
	record := InteractionRecord{
		SessionID:             payload.SessionID,
		TranscriptPath:        payload.TranscriptPath,
		CWD:                   payload.CWD,
		HookEventName:         payload.HookEventName,
		ToolName:              payload.ToolName,
		ToolInput:             payload.ToolInput,
		ToolUseID:             payload.ToolUseID,
		PermissionSuggestions: payload.PermissionSuggestions,
		RecordedAt:            now,
		ExpiresAt:             now.Add(DefaultInteractionTTL),
	}
	record.RequestID, err = interactionRequestID(payload)
	if err != nil {
		return InteractionRecord{}, err
	}
	if interactionDir == "" {
		interactionDir = defaultInteractionDir()
	}
	dir, err := secureInteractionDir(interactionDir, true)
	if err != nil {
		return InteractionRecord{}, err
	}
	if err := writeInteractionRecord(dir, record); err != nil {
		return InteractionRecord{}, err
	}
	return record, nil
}

// RunInteractionObserver is the fail-open hook entry point. A malformed or
// unwritable observation must never answer, deny, or defer Claude's native UI.
func RunInteractionObserver(input io.Reader, interactionDir string) {
	defer func() { _ = recover() }()
	_, _ = ObserveInteraction(input, interactionDir)
}

// RunInteractionCleanup is a second no-output hook path used only to retire
// observer candidates after Claude's native UI handled them. It never removes
// the independent exact-once attempt ledger.
func RunInteractionCleanup(input io.Reader, interactionDir string) {
	defer func() { _ = recover() }()
	_ = CleanupInteraction(input, interactionDir)
}

func CleanupInteraction(input io.Reader, interactionDir string) error {
	raw, err := io.ReadAll(io.LimitReader(input, maxInteractionBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxInteractionBytes {
		return errors.New("Claude interaction cleanup payload is too large")
	}
	var payload struct {
		SessionID     string `json:"session_id"`
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		ToolUseID     string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	payload.SessionID = strings.TrimSpace(payload.SessionID)
	payload.HookEventName = strings.TrimSpace(payload.HookEventName)
	payload.ToolName = strings.TrimSpace(payload.ToolName)
	payload.ToolUseID = strings.TrimSpace(payload.ToolUseID)
	switch payload.HookEventName {
	case "PreToolUse":
		// AskUserQuestion's PreToolUse is the observation point itself. The
		// generic cleanup hook shares this event but must leave that candidate.
		if payload.ToolName == "AskUserQuestion" {
			return nil
		}
		if payload.ToolUseID != "" {
			return RemoveInteraction(interactionDir, payload.ToolUseID)
		}
	case "PostToolUse", "PostToolUseFailure":
		if payload.ToolUseID != "" {
			return RemoveInteraction(interactionDir, payload.ToolUseID)
		}
	case "Stop":
		if payload.SessionID == "" {
			return nil
		}
		records, err := ListInteractions(interactionDir, DefaultInteractionTTL)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.SessionID == payload.SessionID {
				if err := RemoveInteraction(interactionDir, record.RequestID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ReadInteraction returns one still-current record. requestID is never used as
// a path component directly; filenames are fixed-length hashes.
func ReadInteraction(interactionDir, requestID string, maxAge time.Duration) (InteractionRecord, error) {
	if interactionDir == "" {
		interactionDir = defaultInteractionDir()
	}
	dir, err := secureInteractionDir(interactionDir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InteractionRecord{}, ErrInteractionNotFound
		}
		return InteractionRecord{}, err
	}
	record, err := readInteractionRecord(filepath.Join(dir, interactionFilename(requestID)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return InteractionRecord{}, ErrInteractionNotFound
		}
		return InteractionRecord{}, err
	}
	if record.RequestID != requestID {
		return InteractionRecord{}, errors.New("Claude interaction record identity mismatch")
	}
	if interactionExpired(record, maxAge, time.Now().UTC()) {
		_ = removeInteractionPath(dir, requestID)
		return InteractionRecord{}, ErrInteractionExpired
	}
	return record, nil
}

// ListInteractions returns current records newest first and opportunistically
// removes expired records. It ignores foreign filenames in the private spool.
func ListInteractions(interactionDir string, maxAge time.Duration) ([]InteractionRecord, error) {
	if interactionDir == "" {
		interactionDir = defaultInteractionDir()
	}
	dir, err := secureInteractionDir(interactionDir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	records := make([]InteractionRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isInteractionFilename(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		record, readErr := readInteractionRecord(path)
		if readErr != nil {
			if errors.Is(readErr, ErrUnsafeInteractionDir) {
				return nil, readErr
			}
			continue
		}
		if interactionFilename(record.RequestID) != entry.Name() {
			continue
		}
		if interactionExpired(record, maxAge, now) {
			_ = removeInteractionPath(dir, record.RequestID)
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].RecordedAt.After(records[j].RecordedAt)
	})
	return records, nil
}

// RemoveInteraction forgets a consumed native request. Missing records are
// already cleared and therefore succeed.
func RemoveInteraction(interactionDir, requestID string) error {
	if interactionDir == "" {
		interactionDir = defaultInteractionDir()
	}
	dir, err := secureInteractionDir(interactionDir, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return removeInteractionPath(dir, requestID)
}

func defaultInteractionDir() string {
	return filepath.Join("~", ".claude", "agenthalo-interactions")
}

func interactionRequestID(payload interactionHookPayload) (string, error) {
	if payload.ToolUseID != "" {
		return payload.ToolUseID, nil
	}
	normalized, err := json.Marshal(struct {
		SessionID             string `json:"session_id"`
		HookEventName         string `json:"hook_event_name"`
		ToolName              string `json:"tool_name"`
		ToolInput             any    `json:"tool_input"`
		PermissionSuggestions any    `json:"permission_suggestions"`
	}{
		SessionID: payload.SessionID, HookEventName: payload.HookEventName,
		ToolName: payload.ToolName, ToolInput: payload.ToolInput,
		PermissionSuggestions: payload.PermissionSuggestions,
	})
	if err != nil {
		return "", fmt.Errorf("normalize Claude interaction: %w", err)
	}
	sum := sha256.Sum256(normalized)
	return "content-" + hex.EncodeToString(sum[:]), nil
}

func interactionFilename(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(sum[:]) + ".json"
}

func isInteractionFilename(name string) bool {
	if len(name) != sha256.Size*2+len(".json") || !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(name, ".json"))
	return err == nil
}

func secureInteractionDir(path string, create bool) (string, error) {
	path = expandUser(path)
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if err := validateInteractionPathComponents(abs, create); err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", err
		}
		if err := validateInteractionPathComponents(abs, false); err != nil {
			return "", err
		}
		info, err = os.Lstat(abs)
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: path is not a real directory", ErrUnsafeInteractionDir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", fmt.Errorf("%w: directory must be owned by the current user", ErrUnsafeInteractionDir)
	}
	if create {
		if err := os.Chmod(abs, 0o700); err != nil {
			return "", err
		}
	} else if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: permissions must be 0700", ErrUnsafeInteractionDir)
	}
	return abs, nil
}

func validateInteractionPathComponents(path string, allowMissing bool) error {
	volume := filepath.VolumeName(path)
	relative := strings.TrimPrefix(path, volume)
	relative = strings.TrimPrefix(relative, string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && allowMissing {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: path component is a symlink", ErrUnsafeInteractionDir)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: path component is not a directory", ErrUnsafeInteractionDir)
		}
	}
	return nil
}

func writeInteractionRecord(dir string, record InteractionRecord) error {
	path := filepath.Join(dir, interactionFilename(record.RequestID))
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: record target is not a regular file", ErrUnsafeInteractionDir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".interaction-*.tmp")
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
		return fmt.Errorf("persist Claude interaction record: %w", err)
	}
	keep = true
	return nil
}

func readInteractionRecord(path string) (InteractionRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return InteractionRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return InteractionRecord{}, fmt.Errorf("%w: record is not a regular file", ErrUnsafeInteractionDir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return InteractionRecord{}, fmt.Errorf("%w: record permissions must be 0600", ErrUnsafeInteractionDir)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return InteractionRecord{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxInteractionBytes+1))
	if err != nil {
		return InteractionRecord{}, err
	}
	if len(raw) > maxInteractionBytes {
		return InteractionRecord{}, errors.New("Claude interaction record is too large")
	}
	var record InteractionRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return InteractionRecord{}, err
	}
	if record.RequestID == "" || record.SessionID == "" || record.RecordedAt.IsZero() || record.ExpiresAt.IsZero() {
		return InteractionRecord{}, errors.New("Claude interaction record is incomplete")
	}
	return record, nil
}

func interactionExpired(record InteractionRecord, maxAge time.Duration, now time.Time) bool {
	if maxAge <= 0 || maxAge > DefaultInteractionTTL {
		maxAge = DefaultInteractionTTL
	}
	expires := record.RecordedAt.Add(maxAge)
	if record.ExpiresAt.Before(expires) {
		expires = record.ExpiresAt
	}
	return !now.Before(expires)
}

func removeInteractionPath(dir, requestID string) error {
	path := filepath.Join(dir, interactionFilename(requestID))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: record is not a regular file", ErrUnsafeInteractionDir)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := syncDirectoryForDurability(dir); err != nil {
		return fmt.Errorf("persist Claude interaction removal: %w", err)
	}
	return nil
}
