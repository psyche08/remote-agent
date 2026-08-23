package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionArtifactBindingAtomicFailurePreservesPreviousFile(t *testing.T) {
	st := New(t.TempDir())
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-binding",
	}
	paths, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{NativeSessionID: "before"},
		[]SessionArtifactMessage{{Role: "assistant", Text: "answer"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.Binding)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("simulated binding durability failure")
	sessionArtifactWriteFault = func(stage string, path string) error {
		if stage == "after_file_sync" && filepath.Base(path) == sessionBindingFilename {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { sessionArtifactWriteFault = func(string, string) error { return nil } })
	if _, err := st.WriteSessionArtifacts(identity,
		SessionArtifactBinding{NativeSessionID: "after"},
		[]SessionArtifactMessage{{Role: "assistant", Text: "answer"}}); !errors.Is(err, injected) {
		t.Fatalf("atomic binding error=%v", err)
	}
	after, err := os.ReadFile(paths.Binding)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed write replaced binding: before=%q after=%q err=%v", before, after, err)
	}
	entries, err := os.ReadDir(paths.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("failed write left temp file: %s", entry.Name())
		}
	}
}

func TestSessionArtifactWriteTightensExistingPrivateDirectoryModes(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "deepseek", LogicalSessionID: "logical-private",
	}
	paths, err := st.SessionArtifactPaths(identity)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(paths.Directory)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.Directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{},
		[]SessionArtifactMessage{{Role: "user", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, paths.Directory} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s mode=%v err=%v", path, info, err)
		}
	}
	info, err := os.Lstat(paths.Transcript)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("transcript mode=%v err=%v", info, err)
	}
}

func TestSessionArtifactPairRejectsCrashBetweenTranscriptAndBinding(t *testing.T) {
	st := New(t.TempDir())
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-pair",
	}
	paths, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{NativeSessionID: "before"},
		[]SessionArtifactMessage{{Role: "assistant", Text: "before"}})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("simulated crash after transcript rename")
	sessionArtifactWriteFault = func(stage string, path string) error {
		if stage == "after_rename" && filepath.Base(path) == sessionTranscriptFilename {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { sessionArtifactWriteFault = func(string, string) error { return nil } })
	if _, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{NativeSessionID: "after"},
		[]SessionArtifactMessage{{Role: "assistant", Text: "after"}}); !errors.Is(err, injected) {
		t.Fatalf("pair write error=%v", err)
	}
	sessionArtifactWriteFault = func(string, string) error { return nil }
	if _, err := st.ReadSessionArtifactBinding(identity); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("incomplete pair was accepted: paths=%#v err=%v", paths, err)
	}
}

func TestWriteSessionArtifactsIsIdempotent(t *testing.T) {
	st := New(t.TempDir())
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-idempotent",
	}
	binding := SessionArtifactBinding{NativeSessionID: "native", Source: "catpaw_sqlite_legacy"}
	messages := []SessionArtifactMessage{{Role: "user", Text: "same"}}
	paths, err := st.WriteSessionArtifacts(identity, binding, messages)
	if err != nil {
		t.Fatal(err)
	}
	firstTranscript, _ := os.Stat(paths.Transcript)
	firstBinding, _ := os.Stat(paths.Binding)
	injected := errors.New("idempotent write unexpectedly touched disk")
	sessionArtifactWriteFault = func(string, string) error { return injected }
	t.Cleanup(func() { sessionArtifactWriteFault = func(string, string) error { return nil } })
	if _, err := st.WriteSessionArtifacts(identity, binding, messages); err != nil {
		t.Fatal(err)
	}
	secondTranscript, _ := os.Stat(paths.Transcript)
	secondBinding, _ := os.Stat(paths.Binding)
	if !firstTranscript.ModTime().Equal(secondTranscript.ModTime()) ||
		!firstBinding.ModTime().Equal(secondBinding.ModTime()) {
		t.Fatalf("idempotent write changed mtimes: transcript %s -> %s binding %s -> %s",
			firstTranscript.ModTime(), secondTranscript.ModTime(), firstBinding.ModTime(), secondBinding.ModTime())
	}
}

func TestSessionArtifactReadTightensPrivateFileModes(t *testing.T) {
	st := New(t.TempDir())
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-modes",
	}
	paths, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{},
		[]SessionArtifactMessage{{Role: "assistant", Text: "private"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Binding, paths.Transcript} {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ReadSessionArtifactTranscript(identity); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Binding, paths.Transcript} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s mode=%v err=%v", path, info, err)
		}
	}
}

func TestSessionArtifactReadRejectsOversizedExistingFile(t *testing.T) {
	st := New(t.TempDir())
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-oversized",
	}
	paths, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{},
		[]SessionArtifactMessage{{Role: "assistant", Text: "small"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(paths.Transcript, int64(sessionArtifactMaxTranscript)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadSessionArtifactTranscript(identity); err == nil ||
		!strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized transcript err=%v", err)
	}
}
