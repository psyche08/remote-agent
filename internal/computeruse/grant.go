package computeruse

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
	// PublicKeyBytes is the length of the published verifying key: the X9.63
	// uncompressed point (0x04 || X || Y) of a P-256 public key, which is the
	// form SecKeyCreateWithData expects in the plug-in. The plug-in hard-codes
	// the same length, and mac/preflight.sh compares the two.
	PublicKeyBytes = 65
)

// Grants are signed with ECDSA P-256 over SHA-256, and the choice is forced by
// the verifier rather than preferred here.
//
// Ed25519 would be the better primitive and Go signs it with less ceremony, but
// the plug-in must verify through Security.framework, and SecKey's Ed25519
// constants (kSecAttrKeyTypeEd25519, kSecKeyAlgorithmEdDSASignatureMessage…)
// are SPI: exported by Security.tbd, declared in no public header. A mechanism
// bundle that binds a private symbol stops loading the day Apple drops it — and
// this bundle sits in the screensaver-unlock right, where a mechanism that
// cannot load is the lockout direction the design forbids. ECDSA P-256 with
// kSecKeyAlgorithmECDSASignatureMessageX962SHA256 is public API on every
// supported macOS, and Go's SignASN1 emits exactly the X9.62 DER it wants.

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

// Signer mints grants. It holds the P-256 private key and is the only thing in
// the process that can produce a verifiable unlock assertion.
type Signer struct {
	key      *ecdsa.PrivateKey
	deviceID string
}

var (
	ErrNoSigningKey = errors.New("locked use has no signing key")
	// errSigningKeyMalformed names the remedy, because the only way to reach it
	// is a key file this build cannot use — including one written by a build
	// that signed grants with Ed25519.
	errSigningKeyMalformed = errors.New(
		"locked-use signing key is malformed or is not a P-256 key; remove the file to mint a fresh one")
	ErrGrantExpired    = errors.New("grant expired")
	ErrGrantNotYet     = errors.New("grant is not yet valid")
	ErrGrantTTLTooLong = errors.New("grant declares a lifetime beyond the permitted ceiling")
	ErrGrantSignature  = errors.New("grant signature is not valid")
	ErrGrantPurpose    = errors.New("grant purpose does not match")
	ErrGrantDevice     = errors.New("grant device does not match")
	ErrGrantVersion    = errors.New("unsupported grant version")
)

// LoadOrCreateSigner reads the P-256 private key at path, creating one on
// first use. The key is stored as base64 PKCS#8 DER, 0600 in a 0700 directory.
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
		der, decodeErr := base64.StdEncoding.DecodeString(trimSpaceBytes(raw))
		if decodeErr != nil {
			return nil, errSigningKeyMalformed
		}
		parsed, parseErr := x509.ParsePKCS8PrivateKey(der)
		if parseErr != nil {
			return nil, errSigningKeyMalformed
		}
		key, ok := parsed.(*ecdsa.PrivateKey)
		// The curve is checked, not assumed: a key on another curve would sign
		// grants the plug-in — which only ever builds a P-256 key — rejects.
		if !ok || key.Curve != elliptic.P256() {
			return nil, errSigningKeyMalformed
		}
		return &Signer{key: key, deviceID: deviceID}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(der)
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
	return &Signer{key: key, deviceID: deviceID}, nil
}

// PublicKey returns the verifying half as the X9.63 uncompressed point the
// plug-in hands to SecKeyCreateWithData. The private half never leaves this
// process.
func (s *Signer) PublicKey() []byte {
	if s == nil || s.key == nil {
		return nil
	}
	pub, err := s.key.PublicKey.ECDH()
	if err != nil {
		return nil
	}
	return pub.Bytes()
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
	if s == nil || s.key == nil {
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
	sig, err := SignPayload(s.key, raw)
	if err != nil {
		return Grant{}, GrantPayload{}, err
	}
	return Grant{
		Payload:   base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, payload, nil
}

// SignPayload produces the detached signature over the exact payload bytes:
// DER-encoded ECDSA over SHA-256, which is what
// kSecKeyAlgorithmECDSASignatureMessageX962SHA256 verifies on the plug-in side.
//
// It is exported so tests can mint a validly signed but semantically invalid
// grant without reaching into the Signer, and so the signing format lives in
// exactly one place.
func SignPayload(key *ecdsa.PrivateKey, payload []byte) ([]byte, error) {
	if key == nil {
		return nil, ErrNoSigningKey
	}
	digest := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, key, digest[:])
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

// VerifyPayloadSignature mirrors RAVerifySignature in the plug-in: an X9.63
// uncompressed P-256 point verifying a DER ECDSA signature over SHA-256 of the
// payload bytes.
//
// The point goes through ecdh.P256().NewPublicKey, which rejects a point that
// is malformed or not on the curve — the same validation SecKeyCreateWithData
// applies — so this mirror refuses the keys the enforcing copy would refuse
// rather than accepting them and diverging.
func VerifyPayloadSignature(pub, payload, sig []byte) bool {
	if len(pub) != PublicKeyBytes || pub[0] != 0x04 || len(sig) == 0 || len(payload) == 0 {
		return false
	}
	point, err := ecdh.P256().NewPublicKey(pub)
	if err != nil {
		return false
	}
	raw := point.Bytes()
	key := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(raw[1:33]),
		Y:     new(big.Int).SetBytes(raw[33:65]),
	}
	digest := sha256.Sum256(payload)
	return ecdsa.VerifyASN1(key, digest[:], sig)
}

// VerifyGrant is the reference implementation of the check the Authorization
// Plug-in performs. The plugin is the enforcing copy; this one keeps the
// contract honest and testable in CI, where the ObjC bundle cannot run.
//
// Verification deliberately does not trust the grant about its own freshness:
// a declared lifetime longer than MaxGrantTTL is rejected outright rather than
// clamped-and-accepted, so a mis-minted grant fails loudly instead of quietly
// becoming long-lived.
func VerifyGrant(grant Grant, pub []byte, deviceID string, now time.Time) (GrantPayload, error) {
	raw, err := base64.StdEncoding.DecodeString(grant.Payload)
	if err != nil {
		return GrantPayload{}, errors.New("grant payload is malformed")
	}
	sig, err := base64.StdEncoding.DecodeString(grant.Signature)
	if err != nil {
		return GrantPayload{}, errors.New("grant signature is malformed")
	}
	if !VerifyPayloadSignature(pub, raw, sig) {
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
