package computeruse

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A grant is the only thing this process ever hands the unlock flow. It is a
// short-lived, single-use, signed assertion that says "an authorized turn is
// asking to unlock right now" — and nothing else. Specifically:
//
//   - It never contains, derives from, or substitutes for the user's password.
//     The Authorization Plug-in that reads it does not learn a credential; it
//     only decides whether to allow the unlock right it was already asked about.
//   - Its lifetime is measured in seconds and it is minted immediately before
//     an unlock attempt, not held open for the duration of a window. A grant
//     resting on disk is ambient authority that any local process could ride,
//     which is exactly the general-purpose remote unlock this feature must not
//     become.
//   - The verifier independently enforces its own freshness ceiling. A grant
//     that declares a long life is rejected, not honored, so a single leaked or
//     mis-minted grant can never become a durable skeleton key.
const (
	// GrantVersion is the wire version the plugin and this package agree on. A
	// verifier that does not recognize the version refuses the grant.
	GrantVersion = 1
	// GrantPurpose scopes a grant to the screensaver-unlock right. A grant is
	// not a general authorization token and must not verify for anything else.
	GrantPurpose = "screensaver-unlock"
	// GrantFileName is the single file the plugin reads. One name, one grant:
	// there is no queue of pending authorizations to pick from.
	GrantFileName = "grant.json"
	// MinGrantTTL is the fallback when a caller supplies no usable TTL.
	MinGrantTTL = 2 * time.Second
	// MaxGrantTTL is the hard ceiling on a grant's life. Both the minter and
	// the verifier enforce it; the verifier never trusts the grant's own
	// expires_at beyond this bound.
	MaxGrantTTL = 15 * time.Second
	// MaxClockSkew tolerates small clock differences between the minting
	// process and the verifier without opening a meaningful replay window.
	MaxClockSkew = 5 * time.Second
	// NonceBytes is the grant nonce length. The nonce is the single-use key in
	// the verifier's consumed ledger.
	NonceBytes = 16
)

// GrantPayload is the signed portion of a grant. Field names and ordering are
// part of the wire contract with the Authorization Plug-in: the signature
// covers the exact marshaled bytes, so this struct must not gain or reorder
// fields without a GrantVersion bump.
type GrantPayload struct {
	Version   int    `json:"v"`
	Purpose   string `json:"purpose"`
	Nonce     string `json:"nonce"`
	DeviceID  string `json:"device_id"`
	TurnID    string `json:"turn_id"`
	IssuedAt  int64  `json:"issued_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// Grant is the on-disk envelope: an opaque base64 payload plus its detached
// signature. The verifier signature-checks the payload bytes and then parses
// those same bytes, never a separately-parsed copy.
type Grant struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Signer mints grants. It holds the Ed25519 private key and is the only thing
// in the process that can produce a verifiable unlock assertion.
type Signer struct {
	key      ed25519.PrivateKey
	deviceID string
}

var (
	ErrNoSigningKey    = errors.New("locked use has no signing key")
	ErrGrantExpired    = errors.New("grant expired")
	ErrGrantNotYet     = errors.New("grant is not yet valid")
	ErrGrantTTLTooLong = errors.New("grant declares a lifetime beyond the permitted ceiling")
	ErrGrantSignature  = errors.New("grant signature is not valid")
	ErrGrantPurpose    = errors.New("grant purpose does not match")
	ErrGrantDevice     = errors.New("grant device does not match")
	ErrGrantVersion    = errors.New("unsupported grant version")
)

// LoadOrCreateSigner reads the Ed25519 private key at path, creating one on
// first use. The key file is 0600 in a 0700 directory.
//
// Threat-model note: file permissions keep the key from other users, not from a
// process already running as this user. Binding the key to hardware (Secure
// Enclave) with an ACL scoped to the agent's code signature is the stronger
// design and is documented as required hardening in
// docs/computer-use-locked-user.md. Deployments that need to withstand a
// same-user attacker must not treat this file-based key as sufficient.
func LoadOrCreateSigner(path string, deviceID string) (*Signer, error) {
	if path == "" {
		return nil, ErrNoSigningKey
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(path); err == nil {
		seed, decodeErr := base64.StdEncoding.DecodeString(trimSpaceBytes(raw))
		if decodeErr != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("locked-use signing key is malformed")
		}
		return &Signer{key: ed25519.NewKeyFromSeed(seed), deviceID: deviceID}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(seed)
	// Write with O_EXCL so a concurrent starter cannot silently replace a key
	// that another process is already publishing a public half for.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return LoadOrCreateSigner(path, deviceID)
		}
		return nil, err
	}
	if _, err := f.WriteString(encoded + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return &Signer{key: ed25519.NewKeyFromSeed(seed), deviceID: deviceID}, nil
}

// PublicKey returns the verifying half, which is what the Authorization
// Plug-in is provisioned with. The private half never leaves this process.
func (s *Signer) PublicKey() ed25519.PublicKey {
	if s == nil {
		return nil
	}
	return s.key.Public().(ed25519.PublicKey)
}

// PublicKeyBase64 renders the public key for provisioning the plugin.
func (s *Signer) PublicKeyBase64() string {
	if s == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(s.PublicKey())
}

// Mint produces a signed grant valid for ttl. Callers mint immediately before
// an unlock attempt; ttl is clamped to MaxGrantTTL because the verifier will
// reject anything longer regardless.
func (s *Signer) Mint(turnID string, ttl time.Duration, now time.Time) (Grant, GrantPayload, error) {
	if s == nil || len(s.key) == 0 {
		return Grant{}, GrantPayload{}, ErrNoSigningKey
	}
	// An unset or out-of-range TTL falls back to the shortest useful life, not
	// the ceiling: a caller that failed to specify one must not silently get
	// the most permissive grant this code can mint.
	if ttl <= 0 {
		ttl = MinGrantTTL
	}
	if ttl > MaxGrantTTL {
		ttl = MaxGrantTTL
	}
	nonce := make([]byte, NonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return Grant{}, GrantPayload{}, err
	}
	payload := GrantPayload{
		Version:   GrantVersion,
		Purpose:   GrantPurpose,
		Nonce:     hex.EncodeToString(nonce),
		DeviceID:  s.deviceID,
		TurnID:    turnID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Grant{}, GrantPayload{}, err
	}
	return Grant{
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, raw)),
	}, payload, nil
}

// WriteGrant publishes a grant for the Authorization Plug-in to read. The file
// is written to a temporary name and renamed, so the plugin never observes a
// partially written grant it might parse as something else.
func WriteGrant(dir string, grant Grant) error {
	if dir == "" {
		return errors.New("locked use has no grant directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, GrantFileName+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, GrantFileName)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// RemoveGrant withdraws any published grant. It is safe to call when no grant
// exists and is invoked on every window-close path, including failures — a
// grant outliving its window is precisely the ambient authority to avoid.
func RemoveGrant(dir string) error {
	if dir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(dir, GrantFileName))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// A crashed mint can leave the staging file behind; it is not a valid
	// grant, but sweeping it keeps the directory's meaning unambiguous.
	if err := os.Remove(filepath.Join(dir, GrantFileName+".tmp")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ScrubGrants removes every grant artifact in dir. It runs at startup, before
// Locked Use arms, so a grant that survived a crash can never be honored by a
// later unlock attempt.
func ScrubGrants(dir string) error {
	return RemoveGrant(dir)
}

// VerifyGrant is the reference implementation of the check the Authorization
// Plug-in performs. The plugin is the enforcing copy; this one keeps the
// contract honest and testable in CI, where the ObjC bundle cannot run.
//
// Verification deliberately does not trust the grant about its own freshness:
// a declared lifetime longer than MaxGrantTTL is rejected outright rather than
// clamped-and-accepted, so a mis-minted grant fails loudly instead of quietly
// becoming long-lived.
func VerifyGrant(grant Grant, pub ed25519.PublicKey, deviceID string, now time.Time) (GrantPayload, error) {
	raw, err := base64.StdEncoding.DecodeString(grant.Payload)
	if err != nil {
		return GrantPayload{}, errors.New("grant payload is malformed")
	}
	sig, err := base64.StdEncoding.DecodeString(grant.Signature)
	if err != nil {
		return GrantPayload{}, errors.New("grant signature is malformed")
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, raw, sig) {
		return GrantPayload{}, ErrGrantSignature
	}
	// Parse only the bytes the signature covered.
	var payload GrantPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return GrantPayload{}, errors.New("grant payload is malformed")
	}
	if payload.Version != GrantVersion {
		return GrantPayload{}, ErrGrantVersion
	}
	if payload.Purpose != GrantPurpose {
		return GrantPayload{}, ErrGrantPurpose
	}
	if deviceID != "" && payload.DeviceID != deviceID {
		return GrantPayload{}, ErrGrantDevice
	}
	if len(payload.Nonce) != NonceBytes*2 {
		return GrantPayload{}, errors.New("grant nonce is malformed")
	}
	if _, err := hex.DecodeString(payload.Nonce); err != nil {
		return GrantPayload{}, errors.New("grant nonce is malformed")
	}
	issued := time.Unix(payload.IssuedAt, 0)
	expires := time.Unix(payload.ExpiresAt, 0)
	if !expires.After(issued) {
		return GrantPayload{}, ErrGrantExpired
	}
	if expires.Sub(issued) > MaxGrantTTL {
		return GrantPayload{}, ErrGrantTTLTooLong
	}
	if issued.After(now.Add(MaxClockSkew)) {
		return GrantPayload{}, ErrGrantNotYet
	}
	if now.After(expires) {
		return GrantPayload{}, ErrGrantExpired
	}
	return payload, nil
}

// ConsumeNonce records a nonce as used, returning false if it was already
// consumed. The create is O_EXCL so two concurrent verifiers cannot both
// believe they won; a failed write returns an error and the caller must deny
// rather than proceed.
//
// This mirrors the plugin's ledger. The plugin's copy is the enforcing one and
// lives in a root-owned directory.
func ConsumeNonce(ledgerDir string, nonce string, expiresAt time.Time) (bool, error) {
	if ledgerDir == "" {
		return false, errors.New("no nonce ledger directory")
	}
	if len(nonce) != NonceBytes*2 {
		return false, errors.New("grant nonce is malformed")
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return false, errors.New("grant nonce is malformed")
	}
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		return false, err
	}
	path := filepath.Join(ledgerDir, nonce)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	// The entry records only when it may be pruned. It carries no grant body,
	// no key material, and nothing derived from a credential.
	_, writeErr := fmt.Fprintf(f, "%d\n", expiresAt.Unix())
	closeErr := f.Close()
	if writeErr != nil {
		os.Remove(path)
		return false, writeErr
	}
	if closeErr != nil {
		os.Remove(path)
		return false, closeErr
	}
	return true, nil
}

// PruneNonces drops ledger entries whose grants can no longer be valid. Entries
// are removed strictly by expiry, never by count, so pruning can never forget a
// nonce that could still be replayed.
func PruneNonces(ledgerDir string, now time.Time) error {
	entries, err := os.ReadDir(ledgerDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(ledgerDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var expires int64
		if _, err := fmt.Sscanf(string(raw), "%d", &expires); err != nil {
			continue
		}
		// Keep entries until well past expiry so clock jitter cannot resurrect
		// a nonce that is still inside a verifier's acceptance window.
		if now.After(time.Unix(expires, 0).Add(MaxGrantTTL + MaxClockSkew)) {
			os.Remove(path)
		}
	}
	return nil
}

func trimSpaceBytes(b []byte) string {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return string(b[start:end])
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
