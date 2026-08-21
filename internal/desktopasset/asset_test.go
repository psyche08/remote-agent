package desktopasset

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// A build without the helper must still be a working build. The alternative —
// failing to compile, or failing to start — would make every development
// checkout depend on having run a Swift build first.
func TestAbsentAssetIsNotAFailure(t *testing.T) {
	if Embedded() {
		t.Skip("this build embeds a helper")
	}
	if _, err := Bytes(); err != ErrNotEmbedded {
		t.Fatalf("Bytes() err = %v, want ErrNotEmbedded", err)
	}
	if _, err := Materialize(filepath.Join(t.TempDir(), "helper")); err != ErrNotEmbedded {
		t.Fatalf("Materialize err = %v, want ErrNotEmbedded", err)
	}
}

func TestMaterializeWritesAnExecutableAndSkipsRewrites(t *testing.T) {
	if !Embedded() {
		t.Skip("this build embeds no helper")
	}
	path := filepath.Join(t.TempDir(), "agenthalo-desktop")

	replaced, err := Materialize(path)
	if err != nil || !replaced {
		t.Fatalf("first Materialize: replaced=%v err=%v", replaced, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 — launchd cannot start a non-executable", info.Mode().Perm())
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	sum := sha256.Sum256(written)
	want, err := SHA256()
	if err != nil {
		t.Fatalf("SHA256: %v", err)
	}
	if hex.EncodeToString(sum[:]) != want {
		t.Fatal("the materialized helper is not byte-identical to the embedded one")
	}

	// The path is what launchd starts and what TCC records its grants against.
	// Rewriting an identical binary invites a restart and a re-prompt for
	// permissions the user already gave.
	replaced, err = Materialize(path)
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	if replaced {
		t.Error("an unchanged helper was rewritten")
	}
}

// A helper left over from an older release must be replaced, or a device would
// keep running the previous one forever while reporting the new version.
func TestMaterializeReplacesAStaleHelper(t *testing.T) {
	if !Embedded() {
		t.Skip("this build embeds no helper")
	}
	path := filepath.Join(t.TempDir(), "agenthalo-desktop")
	if err := os.WriteFile(path, []byte("an older helper"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	replaced, err := Materialize(path)
	if err != nil || !replaced {
		t.Fatalf("Materialize: replaced=%v err=%v", replaced, err)
	}
	written, _ := os.ReadFile(path)
	sum := sha256.Sum256(written)
	want, _ := SHA256()
	if hex.EncodeToString(sum[:]) != want {
		t.Fatal("a stale helper survived materialization")
	}
}

func TestMaterializeRepairsAnIdenticalNonExecutableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenthalo-desktop")
	data := []byte("signed helper bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	replaced, err := materialize(path, data)
	if err != nil || !replaced {
		t.Fatalf("materialize: replaced=%v err=%v", replaced, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("materialized mode = %v, want regular 0755", info.Mode())
	}
	written, err := os.ReadFile(path)
	if err != nil || string(written) != string(data) {
		t.Fatalf("materialized contents = %q err=%v, want %q", written, err, data)
	}
}

func TestMaterializeReplacesAnIdenticalSymlink(t *testing.T) {
	dir := t.TempDir()
	data := []byte("signed helper bytes")
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "agenthalo-desktop")
	if err := os.WriteFile(target, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	replaced, err := materialize(path, data)
	if err != nil || !replaced {
		t.Fatalf("materialize: replaced=%v err=%v", replaced, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("materialized mode = %v, want regular 0755", info.Mode())
	}
	written, err := os.ReadFile(path)
	if err != nil || string(written) != string(data) {
		t.Fatalf("materialized contents = %q err=%v, want %q", written, err, data)
	}
}

func TestMaterializeSkipsOnlyAValidCurrentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agenthalo-desktop")
	data := []byte("signed helper bytes")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	replaced, err := materialize(path, data)
	if err != nil {
		t.Fatal(err)
	}
	if replaced {
		t.Fatal("a regular 0755 helper with identical bytes was rewritten")
	}
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("materialize replaced the inode it reported unchanged")
	}
}
