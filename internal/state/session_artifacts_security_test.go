package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionArtifactPathsUseOpaqueCompositeIdentity(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-secret-1",
	}
	paths, err := st.SessionArtifactPaths(identity)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256([]byte("m4pro\x00catpaw\x00logical-secret-1"))
	wantDir := filepath.Join(dir, sessionArtifactsDirectory, hex.EncodeToString(wantDigest[:]))
	if paths.Directory != wantDir || paths.Binding != filepath.Join(wantDir, "binding.json") ||
		paths.Transcript != filepath.Join(wantDir, "transcript.md") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	for _, raw := range []string{identity.DeviceID, identity.ProviderID, identity.LogicalSessionID} {
		if strings.Contains(paths.Directory, raw) {
			t.Fatalf("artifact path leaked raw identity %q: %s", raw, paths.Directory)
		}
	}

	otherProvider, err := st.SessionArtifactPaths(SessionArtifactIdentity{
		DeviceID: identity.DeviceID, ProviderID: "deepseek", LogicalSessionID: identity.LogicalSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherDevice, err := st.SessionArtifactPaths(SessionArtifactIdentity{
		DeviceID: "m4mini", ProviderID: identity.ProviderID, LogicalSessionID: identity.LogicalSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.Directory == otherProvider.Directory || paths.Directory == otherDevice.Directory {
		t.Fatal("composite identity did not isolate session artifacts")
	}
}

func TestSessionArtifactIdentityRejectsTraversalAndAmbiguity(t *testing.T) {
	st := New(t.TempDir())
	valid := SessionArtifactIdentity{DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-1"}
	tests := []struct {
		name   string
		mutate func(*SessionArtifactIdentity)
	}{
		{"empty", func(v *SessionArtifactIdentity) { v.LogicalSessionID = "" }},
		{"dot", func(v *SessionArtifactIdentity) { v.LogicalSessionID = ".." }},
		{"slash", func(v *SessionArtifactIdentity) { v.LogicalSessionID = "../victim" }},
		{"backslash", func(v *SessionArtifactIdentity) { v.LogicalSessionID = `..\victim` }},
		{"absolute", func(v *SessionArtifactIdentity) { v.DeviceID = "/tmp/victim" }},
		{"nul", func(v *SessionArtifactIdentity) { v.ProviderID = "cat\x00paw" }},
		{"newline", func(v *SessionArtifactIdentity) { v.ProviderID = "catpaw\nforged" }},
		{"surrounding-space", func(v *SessionArtifactIdentity) { v.LogicalSessionID = " logical-1" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			identity := valid
			tc.mutate(&identity)
			if _, err := st.SessionArtifactPaths(identity); err == nil {
				t.Fatalf("identity %#v was accepted", identity)
			}
		})
	}
	if entries, err := os.ReadDir(st.DataDir()); err == nil && len(entries) != 0 {
		t.Fatalf("rejected identities created files: %#v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestWriteSessionArtifactsUsesPrivateModesAndCanonicalBinding(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	identity := SessionArtifactIdentity{
		DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-1",
	}
	timeNow = func() time.Time {
		return time.Date(2026, 8, 23, 12, 30, 0, 123, time.FixedZone("UTC+8", 8*60*60))
	}
	t.Cleanup(func() { timeNow = func() time.Time { return time.Now() } })

	paths, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{
		NativeSessionID: "native-1", TranscriptID: "transcript-1",
		Source: "catpaw_desktop_transcript", ControlRoute: "desktop_computer_use", Surface: "desktop",
	}, []SessionArtifactMessage{{
		ID: "message-1", Role: " Human ", Kind: " TEXT ",
		Text: "first\r\nsecond\rthird", Timestamp: "2026-08-23T20:30:00+08:00",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, sessionArtifactsDirectory), paths.Directory} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s info=%v err=%v", path, info, err)
		}
	}
	for _, path := range []string{paths.Binding, paths.Transcript} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s info=%v err=%v", path, info, err)
		}
	}

	binding, err := st.ReadSessionArtifactBinding(identity)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Version != sessionArtifactVersion || binding.DeviceID != identity.DeviceID ||
		binding.ProviderID != identity.ProviderID || binding.LogicalSessionID != identity.LogicalSessionID ||
		binding.NativeSessionID != "native-1" || binding.TranscriptID != "transcript-1" ||
		binding.UpdatedAt != "2026-08-23T04:30:00.000000123Z" || len(binding.TranscriptSHA256) != 64 {
		t.Fatalf("unexpected binding: %#v", binding)
	}
	markdown, err := st.ReadSessionArtifactTranscript(identity)
	if err != nil {
		t.Fatal(err)
	}
	text := string(markdown)
	if !strings.Contains(text, "## 1. User") || !strings.Contains(text, "first\nsecond\nthird") ||
		strings.Contains(text, "\r") || strings.Contains(text, identity.LogicalSessionID) {
		t.Fatalf("unexpected markdown:\n%s", text)
	}
	wantDigest := sha256.Sum256(markdown)
	if binding.TranscriptSHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("binding transcript digest=%s want=%x", binding.TranscriptSHA256, wantDigest)
	}
}

func TestNormalizeSessionArtifactMessagesCopiesAndNormalizesInput(t *testing.T) {
	input := []SessionArtifactMessage{
		{Role: "AI", Text: "hello\r\nworld", Timestamp: "2026-08-23T20:00:00+08:00"},
		{Role: "tool", Kind: " TOOL_RESULT ", Name: " Bash ", Result: "ok\r"},
		{Role: "user", Text: ""},
	}
	got, err := NormalizeSessionArtifactMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("normalized messages=%#v", got)
	}
	if got[0].Role != "assistant" || got[0].Kind != "text" || got[0].Text != "hello\nworld" ||
		got[0].Timestamp != "2026-08-23T12:00:00Z" {
		t.Fatalf("first normalized message=%#v", got[0])
	}
	if got[1].Role != "tool" || got[1].Kind != "tool_result" || got[1].Name != "Bash" ||
		got[1].Result != "ok\n" {
		t.Fatalf("second normalized message=%#v", got[1])
	}
	if input[0].Role != "AI" || input[0].Text != "hello\r\nworld" {
		t.Fatalf("normalization mutated provider input: %#v", input[0])
	}
	for _, bad := range []SessionArtifactMessage{
		{Role: "user\nassistant", Text: "forged"},
		{Role: "user", Text: "hidden\x00suffix"},
		{Role: "assistant", Text: "terminal\x1b[31mcontrol"},
		{Role: "assistant", Kind: "text", Text: "answer", Timestamp: "not-a-time"},
	} {
		if _, err := NormalizeSessionArtifactMessages([]SessionArtifactMessage{bad}); err == nil {
			t.Fatalf("unsafe message was accepted: %#v", bad)
		}
	}
}

func TestSessionArtifactMarkdownUsesSafeDynamicFences(t *testing.T) {
	normalized, err := NormalizeSessionArtifactMessages([]SessionArtifactMessage{{
		Role: "assistant", Kind: "text", Text: "before\n```\nafter",
	}})
	if err != nil {
		t.Fatal(err)
	}
	markdown := string(renderSessionArtifactTranscript(normalized))
	if !strings.Contains(markdown, "````\nbefore\n```\nafter\n````") {
		t.Fatalf("unsafe/invalid fence selection:\n%s", markdown)
	}
}

func TestSessionArtifactWritesRejectSymlinksWithoutTouchingNativeTranscript(t *testing.T) {
	for _, target := range []string{"root", "session", "transcript", "binding"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			dataDir := filepath.Join(base, "state")
			if err := os.Mkdir(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			st := New(dataDir)
			identity := SessionArtifactIdentity{DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-1"}
			paths, err := st.SessionArtifactPaths(identity)
			if err != nil {
				t.Fatal(err)
			}
			native := filepath.Join(base, "native-transcript.jsonl")
			const nativeContent = "native transcript must remain immutable\n"
			if err := os.WriteFile(native, []byte(nativeContent), 0o600); err != nil {
				t.Fatal(err)
			}
			switch target {
			case "root":
				if err := os.Symlink(base, filepath.Join(dataDir, sessionArtifactsDirectory)); err != nil {
					t.Fatal(err)
				}
			case "session":
				if err := os.Mkdir(filepath.Join(dataDir, sessionArtifactsDirectory), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(base, paths.Directory); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(filepath.Join(dataDir, sessionArtifactsDirectory), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(paths.Directory, 0o700); err != nil {
					t.Fatal(err)
				}
				path := paths.Transcript
				if target == "binding" {
					path = paths.Binding
				}
				if err := os.Symlink(native, path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = st.WriteSessionArtifacts(identity, SessionArtifactBinding{}, []SessionArtifactMessage{{
				Role: "user", Text: "derived only",
			}})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symbolic link") {
				t.Fatalf("symlink target=%s err=%v", target, err)
			}
			got, readErr := os.ReadFile(native)
			if readErr != nil || string(got) != nativeContent {
				t.Fatalf("native transcript changed: %q err=%v", got, readErr)
			}
		})
	}
}

func TestSessionArtifactAtomicWriteFailurePreservesPreviousFile(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	identity := SessionArtifactIdentity{DeviceID: "m4pro", ProviderID: "catpaw", LogicalSessionID: "logical-1"}
	paths, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{TranscriptID: "before"},
		[]SessionArtifactMessage{{Role: "assistant", Text: "before"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("simulated artifact durability failure")
	sessionArtifactWriteFault = func(stage string, path string) error {
		if stage == "after_file_sync" && filepath.Base(path) == "transcript.md" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { sessionArtifactWriteFault = func(string, string) error { return nil } })
	if _, err := st.WriteSessionArtifacts(identity, SessionArtifactBinding{TranscriptID: "after"},
		[]SessionArtifactMessage{{Role: "assistant", Text: "after"}}); !errors.Is(err, injected) {
		t.Fatalf("atomic transcript error=%v", err)
	}
	after, err := os.ReadFile(paths.Transcript)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed write replaced transcript: before=%q after=%q err=%v", before, after, err)
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
