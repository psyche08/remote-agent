package computeruse

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T, deviceID string) (*Signer, string) {
	t.Helper()
	dir := t.TempDir()
	signer, err := LoadOrCreateSigner(filepath.Join(dir, "signing.key"), deviceID)
	if err != nil {
		t.Fatalf("LoadOrCreateSigner: %v", err)
	}
	return signer, dir
}

func TestMintedGrantVerifies(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	now := time.Now()
	grant, payload, err := signer.Mint("turn-1", 10*time.Second, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := VerifyGrant(grant, signer.PublicKey(), "mac-1", now.Add(time.Second))
	if err != nil {
		t.Fatalf("VerifyGrant: %v", err)
	}
	if got.Nonce != payload.Nonce || got.TurnID != "turn-1" {
		t.Fatalf("verified payload mismatch: %+v", got)
	}
	if got.Purpose != GrantPurpose {
		t.Fatalf("purpose = %q, want %q", got.Purpose, GrantPurpose)
	}
}

// A grant must not carry anything that could stand in for a credential. This
// pins the wire shape so a future field cannot quietly introduce one.
func TestGrantPayloadCarriesNoCredentialFields(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	grant, _, err := signer.Mint("turn-1", 10*time.Second, time.Now())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(grant.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	allowed := map[string]bool{
		"v": true, "purpose": true, "nonce": true,
		"device_id": true, "turn_id": true, "issued_at": true, "expires_at": true,
	}
	for name := range fields {
		if !allowed[name] {
			t.Errorf("grant payload gained unexpected field %q", name)
		}
	}
	for _, banned := range []string{"password", "passwd", "secret", "credential", "token", "keychain"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("grant payload contains %q", banned)
		}
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	now := time.Now()
	grant, payload, _ := signer.Mint("turn-1", 10*time.Second, now)

	// Re-encode the payload with a longer life and keep the original
	// signature: the classic forgery attempt.
	payload.ExpiresAt = now.Add(time.Hour).Unix()
	raw, _ := json.Marshal(payload)
	grant.Payload = base64.StdEncoding.EncodeToString(raw)

	if _, err := VerifyGrant(grant, signer.PublicKey(), "mac-1", now); err == nil {
		t.Fatal("tampered payload verified; signature check is not binding")
	}
}

func TestVerifyRejectsForeignKey(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	other, _ := newTestSigner(t, "mac-1")
	now := time.Now()
	grant, _, _ := signer.Mint("turn-1", 10*time.Second, now)

	if _, err := VerifyGrant(grant, other.PublicKey(), "mac-1", now); err == nil {
		t.Fatal("grant verified against a different key")
	}
}

// The verifier must enforce its own ceiling rather than trusting the grant.
// A grant that declares a long life is rejected outright, not clamped, so a
// mis-minted grant fails loudly instead of becoming a durable skeleton key.
func TestVerifyRejectsOverlongDeclaredTTL(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	now := time.Now()

	// Sign a payload directly so the overlong TTL carries a *valid* signature.
	payload := GrantPayload{
		Version: GrantVersion, Purpose: GrantPurpose,
		Nonce:    strings.Repeat("ab", NonceBytes),
		DeviceID: "mac-1", TurnID: "turn-1",
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
	}
	raw, _ := json.Marshal(payload)
	grant := Grant{
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signerKey(t, signer), raw)),
	}
	_, err := VerifyGrant(grant, signer.PublicKey(), "mac-1", now)
	if err != ErrGrantTTLTooLong {
		t.Fatalf("err = %v, want ErrGrantTTLTooLong", err)
	}
}

func TestVerifyRejectsExpiredAndFutureGrants(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	now := time.Now()
	grant, _, _ := signer.Mint("turn-1", 5*time.Second, now)

	if _, err := VerifyGrant(grant, signer.PublicKey(), "mac-1", now.Add(time.Minute)); err != ErrGrantExpired {
		t.Fatalf("expired grant: err = %v, want ErrGrantExpired", err)
	}
	if _, err := VerifyGrant(grant, signer.PublicKey(), "mac-1", now.Add(-time.Hour)); err != ErrGrantNotYet {
		t.Fatalf("future grant: err = %v, want ErrGrantNotYet", err)
	}
}

func TestVerifyRejectsWrongDeviceAndPurpose(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	now := time.Now()
	grant, _, _ := signer.Mint("turn-1", 10*time.Second, now)

	if _, err := VerifyGrant(grant, signer.PublicKey(), "mac-2", now); err != ErrGrantDevice {
		t.Fatalf("err = %v, want ErrGrantDevice", err)
	}

	payload := GrantPayload{
		Version: GrantVersion, Purpose: "something-else",
		Nonce:    strings.Repeat("cd", NonceBytes),
		DeviceID: "mac-1", TurnID: "turn-1",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Second).Unix(),
	}
	raw, _ := json.Marshal(payload)
	wrongPurpose := Grant{
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signerKey(t, signer), raw)),
	}
	if _, err := VerifyGrant(wrongPurpose, signer.PublicKey(), "mac-1", now); err != ErrGrantPurpose {
		t.Fatalf("err = %v, want ErrGrantPurpose", err)
	}
}

// Single-use is the property that stops replay. A nonce may be consumed once
// and only once, even under concurrency.
func TestConsumeNonceIsSingleUse(t *testing.T) {
	dir := t.TempDir()
	nonce := strings.Repeat("0f", NonceBytes)
	expires := time.Now().Add(10 * time.Second)

	ok, err := ConsumeNonce(dir, nonce, expires)
	if err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v, want true/nil", ok, err)
	}
	ok, err = ConsumeNonce(dir, nonce, expires)
	if err != nil {
		t.Fatalf("second consume returned error: %v", err)
	}
	if ok {
		t.Fatal("nonce consumed twice; replay is possible")
	}
}

func TestConsumeNonceConcurrentSingleWinner(t *testing.T) {
	dir := t.TempDir()
	nonce := strings.Repeat("7a", NonceBytes)
	expires := time.Now().Add(10 * time.Second)

	const racers = 16
	results := make(chan bool, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			ok, _ := ConsumeNonce(dir, nonce, expires)
			results <- ok
		}()
	}
	close(start)
	won := 0
	for i := 0; i < racers; i++ {
		if <-results {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent consumers succeeded, want exactly 1", won)
	}
}

func TestConsumeNonceRejectsMalformedNonce(t *testing.T) {
	dir := t.TempDir()
	expires := time.Now().Add(10 * time.Second)
	// A nonce becomes a ledger filename; a traversal attempt must not escape.
	for _, bad := range []string{"", "short", "../../etc/passwd", strings.Repeat("zz", NonceBytes)} {
		if ok, err := ConsumeNonce(dir, bad, expires); ok || err == nil {
			t.Errorf("ConsumeNonce(%q): ok=%v err=%v, want refusal", bad, ok, err)
		}
	}
}

// Pruning must be driven strictly by expiry. Dropping an entry that could still
// be inside a verifier's acceptance window would re-enable replay.
func TestPruneNoncesKeepsEntriesUntilSafelyExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fresh := strings.Repeat("11", NonceBytes)
	stale := strings.Repeat("22", NonceBytes)

	if _, err := ConsumeNonce(dir, fresh, now.Add(10*time.Second)); err != nil {
		t.Fatalf("consume fresh: %v", err)
	}
	if _, err := ConsumeNonce(dir, stale, now.Add(-time.Hour)); err != nil {
		t.Fatalf("consume stale: %v", err)
	}
	if err := PruneNonces(dir, now); err != nil {
		t.Fatalf("PruneNonces: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fresh)); err != nil {
		t.Error("pruned a nonce that is still within its acceptance window")
	}
	if _, err := os.Stat(filepath.Join(dir, stale)); !os.IsNotExist(err) {
		t.Error("long-expired nonce was not pruned")
	}

	// A just-expired nonce must survive until past the skew allowance.
	if err := PruneNonces(dir, now.Add(11*time.Second)); err != nil {
		t.Fatalf("PruneNonces: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fresh)); err != nil {
		t.Error("pruned a just-expired nonce still inside the skew window")
	}
}

func TestWriteAndScrubGrant(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	dir := t.TempDir()
	grant, _, _ := signer.Mint("turn-1", 10*time.Second, time.Now())

	if err := WriteGrant(dir, grant); err != nil {
		t.Fatalf("WriteGrant: %v", err)
	}
	path := filepath.Join(dir, GrantFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("grant not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("grant mode = %o, want 600", perm)
	}

	// A crash must not leave a usable grant behind for a later unlock.
	if err := ScrubGrants(dir); err != nil {
		t.Fatalf("ScrubGrants: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("grant survived the scrub")
	}
	// Scrub is idempotent; a restart on a clean device must not error.
	if err := ScrubGrants(dir); err != nil {
		t.Fatalf("second ScrubGrants: %v", err)
	}
}

func TestSigningKeyIsPersistedWithRestrictivePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "signing.key")
	first, err := LoadOrCreateSigner(path, "mac-1")
	if err != nil {
		t.Fatalf("LoadOrCreateSigner: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key mode = %o, want 600", perm)
	}
	// A restart must reuse the same key, or every previously provisioned
	// plugin would stop verifying.
	second, err := LoadOrCreateSigner(path, "mac-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if first.PublicKeyBase64() != second.PublicKeyBase64() {
		t.Fatal("signing key changed across reload")
	}
}

// signerKey reaches the private key for tests that must produce a validly
// signed but semantically invalid grant.
func signerKey(t *testing.T, s *Signer) ed25519.PrivateKey {
	t.Helper()
	return s.key
}
