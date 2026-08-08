//go:build darwin

package desktopasset

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The single-artifact deploy model rests on this: a code signature lives inside
// the Mach-O, so a binary carried through go:embed and written back out is
// still the signed binary that was notarized. If that were not true, the helper
// would lose its identity on every update and with it every TCC grant.
func TestMaterializedHelperKeepsItsSignature(t *testing.T) {
	if !Embedded() {
		t.Skip("this build embeds no helper")
	}
	path := filepath.Join(t.TempDir(), "remote-agent-desktop")
	if _, err := Materialize(path); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	out, err := exec.Command("codesign", "--verify", "--strict", "--verbose=2", path).CombinedOutput()
	if err != nil {
		t.Fatalf("the materialized helper does not verify: %v\n%s", err, out)
	}
}
