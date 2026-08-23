package state

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
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	sessionArtifactsDirectory    = "session-artifacts"
	sessionArtifactVersion       = 1
	sessionBindingFilename       = "binding.json"
	sessionTranscriptFilename    = "transcript.md"
	sessionArtifactMaxMessages   = 10000
	sessionArtifactMaxBodyBytes  = 16 * 1024 * 1024
	sessionArtifactMaxTotalBytes = 64 * 1024 * 1024
	sessionArtifactMaxTranscript = 96 * 1024 * 1024
	sessionArtifactMaxBinding    = 64 * 1024
)

// sessionArtifactWriteFault is a deterministic durability seam for tests.
var sessionArtifactWriteFault = func(string, string) error { return nil }

// SessionArtifactIdentity is the complete isolation key for a derived session
// artifact. None of these values is ever used as a path component: the on-disk
// directory is sha256(device_id + NUL + provider_id + NUL + logical_session_id).
type SessionArtifactIdentity struct {
	DeviceID         string
	ProviderID       string
	LogicalSessionID string
}

// SessionArtifactPaths names AgentHalo-owned derived files. It must never be
// pointed at, or substituted for, a provider's native transcript.
type SessionArtifactPaths struct {
	Directory  string
	Binding    string
	Transcript string
}

// SessionArtifactBinding is the private durable mapping accompanying a
// derived transcript. Identity, version, update time, and transcript digest
// are assigned by Store and caller-supplied values for them are ignored.
type SessionArtifactBinding struct {
	Version          int    `json:"version"`
	DeviceID         string `json:"device_id"`
	ProviderID       string `json:"provider_id"`
	LogicalSessionID string `json:"logical_session_id"`
	NativeSessionID  string `json:"native_session_id,omitempty"`
	TranscriptID     string `json:"transcript_id,omitempty"`
	Source           string `json:"source,omitempty"`
	ControlRoute     string `json:"control_route,omitempty"`
	Surface          string `json:"surface,omitempty"`
	TranscriptSHA256 string `json:"transcript_sha256,omitempty"`
	UpdatedAt        string `json:"updated_at"`
}

// SessionArtifactMessage is provider-neutral, already-derived message input.
// The artifact layer deliberately accepts no native transcript path and never
// opens a provider-owned transcript for writing.
type SessionArtifactMessage struct {
	ID        string `json:"id,omitempty"`
	Role      string `json:"role"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Text      string `json:"text,omitempty"`
	Result    string `json:"result,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// SessionArtifactPaths derives opaque paths without creating anything.
func (s *Store) SessionArtifactPaths(identity SessionArtifactIdentity) (SessionArtifactPaths, error) {
	digest, err := sessionArtifactDigest(identity)
	if err != nil {
		return SessionArtifactPaths{}, err
	}
	dataDir, err := filepath.Abs(s.dataDir)
	if err != nil {
		return SessionArtifactPaths{}, fmt.Errorf("resolve state data directory: %w", err)
	}
	root := filepath.Join(dataDir, sessionArtifactsDirectory)
	directory := filepath.Join(root, digest)
	rel, err := filepath.Rel(root, directory)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return SessionArtifactPaths{}, fmt.Errorf("derived session artifact path escapes root")
	}
	return SessionArtifactPaths{
		Directory:  directory,
		Binding:    filepath.Join(directory, sessionBindingFilename),
		Transcript: filepath.Join(directory, sessionTranscriptFilename),
	}, nil
}

// WriteSessionArtifacts publishes a normalized derived transcript first and
// binding.json last. Both files are independently atomic; the binding digest
// identifies the exact transcript bytes it accompanies.
func (s *Store) WriteSessionArtifacts(
	identity SessionArtifactIdentity,
	binding SessionArtifactBinding,
	messages []SessionArtifactMessage,
) (SessionArtifactPaths, error) {
	normalized, err := NormalizeSessionArtifactMessages(messages)
	if err != nil {
		return SessionArtifactPaths{}, err
	}
	binding, err = normalizeSessionArtifactBinding(binding)
	if err != nil {
		return SessionArtifactPaths{}, err
	}
	paths, err := s.SessionArtifactPaths(identity)
	if err != nil {
		return SessionArtifactPaths{}, err
	}
	transcript := renderSessionArtifactTranscript(normalized)
	if len(transcript) > sessionArtifactMaxTranscript {
		return SessionArtifactPaths{}, errors.New("session artifact transcript exceeds the size limit")
	}
	digest := sha256.Sum256(transcript)
	binding.Version = sessionArtifactVersion
	binding.DeviceID = identity.DeviceID
	binding.ProviderID = identity.ProviderID
	binding.LogicalSessionID = identity.LogicalSessionID
	binding.TranscriptSHA256 = hex.EncodeToString(digest[:])
	binding.UpdatedAt = nowISO()
	bindingJSON, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return SessionArtifactPaths{}, fmt.Errorf("marshal session artifact binding: %w", err)
	}
	bindingJSON = append(bindingJSON, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepareSessionArtifactDirectoryUnlocked(paths, true); err != nil {
		return SessionArtifactPaths{}, err
	}
	// Reject both final entries before publishing either file. A symlink must
	// never redirect or stand in for an AgentHalo-owned artifact.
	if err := rejectSessionArtifactEntry(paths.Transcript); err != nil {
		return SessionArtifactPaths{}, err
	}
	if err := rejectSessionArtifactEntry(paths.Binding); err != nil {
		return SessionArtifactPaths{}, err
	}
	// Polling previews are frequent. Preserve the files and their mtimes when
	// the normalized transcript and every binding field are already current.
	transcriptCurrent := false
	if currentTranscript, readErr := readSessionArtifactFile(paths.Transcript); readErr == nil &&
		bytes.Equal(currentTranscript, transcript) {
		transcriptCurrent = true
		if currentBindingJSON, bindingErr := readSessionArtifactFile(paths.Binding); bindingErr == nil {
			var currentBinding SessionArtifactBinding
			if json.Unmarshal(currentBindingJSON, &currentBinding) == nil &&
				sessionArtifactBindingsEquivalent(currentBinding, binding) {
				return paths, nil
			}
		}
	}
	if !transcriptCurrent {
		if err := writeSessionArtifactAtomic(paths.Directory, paths.Transcript, transcript); err != nil {
			return SessionArtifactPaths{}, err
		}
	}
	if err := writeSessionArtifactAtomic(paths.Directory, paths.Binding, bindingJSON); err != nil {
		return SessionArtifactPaths{}, err
	}
	return paths, nil
}

func (s *Store) ReadSessionArtifactBinding(identity SessionArtifactIdentity) (SessionArtifactBinding, error) {
	paths, err := s.SessionArtifactPaths(identity)
	if err != nil {
		return SessionArtifactBinding{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepareSessionArtifactDirectoryUnlocked(paths, false); err != nil {
		return SessionArtifactBinding{}, err
	}
	binding, _, err := readSessionArtifactPair(paths, identity)
	return binding, err
}

func (s *Store) ReadSessionArtifactTranscript(identity SessionArtifactIdentity) ([]byte, error) {
	paths, err := s.SessionArtifactPaths(identity)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.prepareSessionArtifactDirectoryUnlocked(paths, false); err != nil {
		return nil, err
	}
	_, transcript, err := readSessionArtifactPair(paths, identity)
	return transcript, err
}

func readSessionArtifactPair(
	paths SessionArtifactPaths, identity SessionArtifactIdentity,
) (SessionArtifactBinding, []byte, error) {
	b, err := readSessionArtifactFile(paths.Binding)
	if err != nil {
		return SessionArtifactBinding{}, nil, err
	}
	var binding SessionArtifactBinding
	if err := json.Unmarshal(b, &binding); err != nil {
		return SessionArtifactBinding{}, nil, fmt.Errorf("parse %s: %w", paths.Binding, err)
	}
	if binding.Version != sessionArtifactVersion || binding.DeviceID != identity.DeviceID ||
		binding.ProviderID != identity.ProviderID || binding.LogicalSessionID != identity.LogicalSessionID {
		return SessionArtifactBinding{}, nil, errors.New("session artifact binding identity mismatch")
	}
	if binding.TranscriptSHA256 == "" {
		return SessionArtifactBinding{}, nil, errors.New("session artifact binding has no transcript digest")
	}
	transcript, err := readSessionArtifactFile(paths.Transcript)
	if err != nil {
		return SessionArtifactBinding{}, nil, fmt.Errorf("read bound session artifact transcript: %w", err)
	}
	digest := sha256.Sum256(transcript)
	if binding.TranscriptSHA256 != hex.EncodeToString(digest[:]) {
		return SessionArtifactBinding{}, nil, errors.New("session artifact transcript digest mismatch")
	}
	return binding, transcript, nil
}

// NormalizeSessionArtifactMessages creates a canonical copy without changing
// provider-owned message values. It rejects hidden NUL/control injection,
// canonicalizes roles and timestamps, normalizes newlines, and omits empty
// placeholder rows.
func NormalizeSessionArtifactMessages(messages []SessionArtifactMessage) ([]SessionArtifactMessage, error) {
	if len(messages) > sessionArtifactMaxMessages {
		return nil, fmt.Errorf("session artifact has too many messages")
	}
	out := make([]SessionArtifactMessage, 0, len(messages))
	totalBodyBytes := 0
	for index, message := range messages {
		var normalized SessionArtifactMessage
		var err error
		if normalized.ID, err = normalizeArtifactInline("message id", message.ID, true); err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		if normalized.Name, err = normalizeArtifactInline("message name", message.Name, true); err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		rawRole, err := normalizeArtifactInline("message role", message.Role, true)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		switch strings.ToLower(rawRole) {
		case "human":
			normalized.Role = "user"
		case "ai", "model":
			normalized.Role = "assistant"
		case "user", "assistant", "system", "developer", "tool":
			normalized.Role = strings.ToLower(rawRole)
		case "":
			normalized.Role = "unknown"
		default:
			normalized.Role = "unknown"
		}
		normalized.Text, err = normalizeArtifactBody("message text", message.Text)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		normalized.Result, err = normalizeArtifactBody("message result", message.Result)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		totalBodyBytes += len(normalized.Text) + len(normalized.Result)
		if totalBodyBytes > sessionArtifactMaxTotalBytes {
			return nil, fmt.Errorf("session artifact message bodies exceed the total size limit")
		}
		kind, err := normalizeArtifactToken("message kind", message.Kind)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		if kind == "" {
			if normalized.Result != "" && normalized.Text == "" {
				kind = "tool_result"
			} else {
				kind = "text"
			}
		}
		normalized.Kind = kind
		if message.Timestamp != "" {
			timestamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(message.Timestamp))
			if err != nil {
				return nil, fmt.Errorf("message %d: invalid timestamp: %w", index, err)
			}
			normalized.Timestamp = timestamp.UTC().Format(time.RFC3339Nano)
		}
		if normalized.Text == "" && normalized.Result == "" && normalized.Name == "" {
			continue
		}
		out = append(out, normalized)
	}
	return out, nil
}

func renderSessionArtifactTranscript(messages []SessionArtifactMessage) []byte {
	var output strings.Builder
	output.WriteString("# Session Transcript\n\n")
	for index, message := range messages {
		fmt.Fprintf(&output, "## %d. %s\n\n", index+1, sessionArtifactRoleTitle(message.Role))
		fmt.Fprintf(&output, "- Kind: %s\n", escapeArtifactMarkdownInline(message.Kind))
		if message.Name != "" {
			fmt.Fprintf(&output, "- Name: %s\n", escapeArtifactMarkdownInline(message.Name))
		}
		if message.ID != "" {
			fmt.Fprintf(&output, "- Message ID: %s\n", escapeArtifactMarkdownInline(message.ID))
		}
		if message.Timestamp != "" {
			fmt.Fprintf(&output, "- Time: %s\n", escapeArtifactMarkdownInline(message.Timestamp))
		}
		output.WriteByte('\n')
		if message.Text != "" {
			writeArtifactMarkdownFence(&output, message.Text)
		}
		if message.Result != "" {
			output.WriteString("**Result**\n\n")
			writeArtifactMarkdownFence(&output, message.Result)
		}
	}
	return []byte(output.String())
}

func (s *Store) prepareSessionArtifactDirectoryUnlocked(paths SessionArtifactPaths, create bool) error {
	dataDir, err := filepath.Abs(s.dataDir)
	if err != nil {
		return err
	}
	if create {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return fmt.Errorf("create state data directory: %w", err)
		}
	}
	if err := requireSessionArtifactDirectory(dataDir, 0, false); err != nil {
		return err
	}
	root := filepath.Join(dataDir, sessionArtifactsDirectory)
	if err := requireSessionArtifactDirectory(root, 0o700, create); err != nil {
		return err
	}
	if err := requireSessionArtifactDirectory(paths.Directory, 0o700, create); err != nil {
		return err
	}
	return nil
}

func requireSessionArtifactDirectory(path string, mode os.FileMode, create bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && create {
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("create session artifact directory %s: %w", path, err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session artifact directory is a symbolic link: %s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("session artifact path is not a directory: %s", path)
	}
	if mode != 0 && info.Mode().Perm() != mode {
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("secure session artifact directory %s: %w", path, err)
		}
	}
	return nil
}

func writeSessionArtifactAtomic(directory string, path string, data []byte) error {
	if filepath.Dir(path) != directory {
		return fmt.Errorf("session artifact target escapes its directory")
	}
	if err := rejectSessionArtifactEntry(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func(primary error, closeFile bool) error {
		if closeFile {
			primary = errors.Join(primary, f.Close())
		}
		if removeErr := os.Remove(tmp); removeErr != nil && !os.IsNotExist(removeErr) {
			primary = errors.Join(primary, fmt.Errorf("remove atomic temp %s: %w", tmp, removeErr))
		}
		return primary
	}
	if err := f.Chmod(0o600); err != nil {
		return cleanup(err, true)
	}
	if written, err := f.Write(data); err != nil {
		return cleanup(err, true)
	} else if written != len(data) {
		return cleanup(io.ErrShortWrite, true)
	}
	if err := sessionArtifactWriteFault("after_write", path); err != nil {
		return cleanup(err, true)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err, true)
	}
	if err := sessionArtifactWriteFault("after_file_sync", path); err != nil {
		return cleanup(err, true)
	}
	if err := f.Close(); err != nil {
		return cleanup(err, false)
	}
	if err := requireSessionArtifactDirectory(directory, 0o700, false); err != nil {
		return cleanup(err, false)
	}
	if err := rejectSessionArtifactEntry(path); err != nil {
		return cleanup(err, false)
	}
	if err := os.Rename(tmp, path); err != nil {
		return cleanup(err, false)
	}
	if err := sessionArtifactWriteFault("after_rename", path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func readSessionArtifactFile(path string) ([]byte, error) {
	if err := rejectSessionArtifactEntry(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("session artifact is not a regular file: %s", path), f.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.Join(fmt.Errorf("session artifact has an unexpected owner: %s", path), f.Close())
	}
	if info.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			return nil, errors.Join(fmt.Errorf("secure session artifact file %s: %w", path, err), f.Close())
		}
	}
	limit := int64(sessionArtifactMaxTranscript)
	if filepath.Base(path) == sessionBindingFilename {
		limit = int64(sessionArtifactMaxBinding)
	}
	if info.Size() > limit {
		return nil, errors.Join(fmt.Errorf("session artifact exceeds the size limit: %s", path), f.Close())
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.Join(fmt.Errorf("session artifact exceeds the size limit: %s", path), f.Close())
	}
	return b, errors.Join(err, f.Close())
}

func rejectSessionArtifactEntry(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session artifact is a symbolic link: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("session artifact is not a regular file: %s", path)
	}
	return nil
}

func sessionArtifactDigest(identity SessionArtifactIdentity) (string, error) {
	for label, value := range map[string]string{
		"device_id": identity.DeviceID, "provider_id": identity.ProviderID,
		"logical_session_id": identity.LogicalSessionID,
	} {
		if err := validateSessionArtifactIdentityPart(label, value); err != nil {
			return "", err
		}
	}
	payload := identity.DeviceID + "\x00" + identity.ProviderID + "\x00" + identity.LogicalSessionID
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:]), nil
}

func validateSessionArtifactIdentityPart(label string, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > 1024 || !utf8.ValidString(value) {
		return fmt.Errorf("invalid %s", label)
	}
	if strings.TrimSpace(value) != value || value == "." || value == ".." ||
		filepath.IsAbs(value) || strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("unsafe %s", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("unsafe %s", label)
		}
	}
	return nil
}

func normalizeSessionArtifactBinding(binding SessionArtifactBinding) (SessionArtifactBinding, error) {
	var err error
	for label, value := range map[string]*string{
		"native_session_id": &binding.NativeSessionID,
		"transcript_id":     &binding.TranscriptID,
		"source":            &binding.Source,
		"control_route":     &binding.ControlRoute,
		"surface":           &binding.Surface,
	} {
		*value, err = normalizeArtifactInline(label, *value, true)
		if err != nil {
			return SessionArtifactBinding{}, err
		}
	}
	binding.Version = 0
	binding.DeviceID = ""
	binding.ProviderID = ""
	binding.LogicalSessionID = ""
	binding.TranscriptSHA256 = ""
	binding.UpdatedAt = ""
	return binding, nil
}

func normalizeArtifactInline(label string, value string, optional bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && optional {
		return "", nil
	}
	if len(value) > 4096 || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid %s", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("unsafe %s", label)
		}
	}
	return value, nil
}

func normalizeArtifactToken(label string, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') &&
			r != '_' && r != '-' && r != '.' && r != ':' {
			return "", fmt.Errorf("unsafe %s", label)
		}
	}
	return value, nil
}

func normalizeArtifactBody(label string, value string) (string, error) {
	if !utf8.ValidString(value) || len(value) > sessionArtifactMaxBodyBytes {
		return "", fmt.Errorf("unsafe %s", label)
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", fmt.Errorf("unsafe %s", label)
		}
	}
	return value, nil
}

func sessionArtifactBindingsEquivalent(a SessionArtifactBinding, b SessionArtifactBinding) bool {
	a.UpdatedAt = ""
	b.UpdatedAt = ""
	return a == b
}

func sessionArtifactRoleTitle(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "system":
		return "System"
	case "developer":
		return "Developer"
	case "tool":
		return "Tool"
	default:
		return "Unknown"
	}
}

func escapeArtifactMarkdownInline(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]",
	)
	return replacer.Replace(value)
}

func writeArtifactMarkdownFence(output *strings.Builder, value string) {
	longest, current := 0, 0
	for _, r := range value {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	if longest < 2 {
		longest = 2
	}
	fence := strings.Repeat("`", longest+1)
	output.WriteString(fence)
	output.WriteByte('\n')
	output.WriteString(value)
	if !strings.HasSuffix(value, "\n") {
		output.WriteByte('\n')
	}
	output.WriteString(fence)
	output.WriteString("\n\n")
}
