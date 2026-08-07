package computeruse

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The grant contract has two verifiers in two languages: VerifyGrant here and
// RAVerifySignature in mac/authorization-plugin/RemoteAgentLockedUse.m. Only
// the Objective-C one enforces anything, and it cannot run in CI. Nothing else
// in the suite can tell the difference between "both sides agree" and "both
// sides are independently self-consistent and reject each other" — which is a
// silent failure: the agent mints, the plug-in refuses, and the only symptom is
// a Mac that never unlocks.
//
// This test mints a grant exactly as the controller does and checks the Go
// mirror accepts it. When RA_INTEROP_VECTOR_OUT is set — mac/preflight.sh sets
// it on the target Mac — it also writes the vector so the real Objective-C
// verifier can be run against bytes this package actually produced.
//
// The file is four lines, in order:
//
//  1. base64 public key (the one that must verify)
//  2. base64 payload
//  3. base64 signature
//  4. base64 public key of a different signer (must NOT verify)
//
// Line 4 is what makes the check decisive. Without a case the verifier must
// reject, a verifier that returned "allowed" unconditionally would pass.
func TestInteropVector(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-interop")
	other, _ := newTestSigner(t, "mac-interop")

	now := time.Now()
	grant, _, err := signer.Mint("turn-interop", 10*time.Second, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := VerifyGrant(grant, signer.PublicKey(), "mac-interop", now); err != nil {
		t.Fatalf("own grant did not verify against own key: %v", err)
	}
	if _, err := VerifyGrant(grant, other.PublicKey(), "mac-interop", now); err != ErrGrantSignature {
		t.Fatalf("grant verified under a foreign key: err = %v, want ErrGrantSignature", err)
	}
	if len(signer.PublicKey()) != PublicKeyBytes {
		t.Fatalf("public key is %d bytes, want %d", len(signer.PublicKey()), PublicKeyBytes)
	}

	out := os.Getenv("RA_INTEROP_VECTOR_OUT")
	if out == "" {
		return
	}
	lines := strings.Join([]string{
		signer.PublicKeyBase64(),
		grant.Payload,
		grant.Signature,
		other.PublicKeyBase64(),
	}, "\n") + "\n"
	if err := os.WriteFile(out, []byte(lines), 0o600); err != nil {
		t.Fatalf("write interop vector: %v", err)
	}
}
