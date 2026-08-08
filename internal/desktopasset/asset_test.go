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
	path := filepath.Join(t.TempDir(), "remote-agent-desktop")

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
	path := filepath.Join(t.TempDir(), "remote-agent-desktop")
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
