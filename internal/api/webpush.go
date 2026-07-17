package api

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const webPushRecordSize = 4096

func newWebPushRequest(sub pushSub, vapid map[string]string, payload []byte) (*http.Request, error) {
	priv, err := parseVAPIDPrivateKey(vapid["private_pem"])
	if err != nil {
		return nil, err
	}
	body, err := encryptWebPush(payload, sub.P256DH, sub.Auth)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(sub.Endpoint)
	if err != nil {
		return nil, err
	}
	if endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("push endpoint is not an absolute URL")
	}
	aud := endpoint.Scheme + "://" + endpoint.Host
	jwt, err := signVAPIDJWT(priv, aud, pushVAPIDSub, time.Now().Add(12*time.Hour))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "WebPush "+jwt)
	req.Header.Set("Crypto-Key", "p256ecdsa="+vapid["public_b64"])
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	return req, nil
}

func encryptWebPush(plaintext []byte, receiverKeyB64 string, authSecretB64 string) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, errors.New("web push payload is empty")
	}
	receiverPub, rx, ry, err := decodeP256Public(receiverKeyB64)
	if err != nil {
		return nil, err
	}
	authSecret, err := b64URLDecode(authSecretB64)
	if err != nil {
		return nil, fmt.Errorf("decode auth secret: %w", err)
	}
	senderPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptWebPushWith(plaintext, receiverPub, rx, ry, authSecret, senderPriv, salt)
}

func encryptWebPushWith(plaintext []byte, receiverPub []byte, rx *big.Int, ry *big.Int, authSecret []byte, senderPriv *ecdsa.PrivateKey, salt []byte) ([]byte, error) {
	if len(salt) != 16 {
		return nil, errors.New("web push salt must be 16 bytes")
	}
	if len(authSecret) == 0 {
		return nil, errors.New("web push auth secret is empty")
	}
	if senderPriv == nil || senderPriv.Curve != elliptic.P256() {
		return nil, errors.New("sender key must be P-256")
	}
	sharedX, _ := elliptic.P256().ScalarMult(rx, ry, leftPad(senderPriv.D.Bytes(), 32))
	if sharedX == nil {
		return nil, errors.New("ECDH failed")
	}
	shared := leftPad(sharedX.Bytes(), 32)
	senderPub := elliptic.Marshal(elliptic.P256(), senderPriv.PublicKey.X, senderPriv.PublicKey.Y)

	context := make([]byte, 0, len("WebPush: info\x00")+len(receiverPub)+len(senderPub))
	context = append(context, []byte("WebPush: info\x00")...)
	context = append(context, receiverPub...)
	context = append(context, senderPub...)

	authPRK := hkdfExtract(authSecret, shared)
	ikm := hkdfExpand(authPRK, context, 32)
	contentPRK := hkdfExtract(salt, ikm)
	cek := hkdfExpand(contentPRK, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfExpand(contentPRK, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	chunkSize := webPushRecordSize - aead.Overhead() - 1
	if chunkSize <= 0 {
		return nil, errors.New("record size is too small")
	}
	encrypted := []byte{}
	var counter uint64
	for off := 0; off < len(plaintext); off += chunkSize {
		end := off + chunkSize
		if end > len(plaintext) {
			end = len(plaintext)
		}
		last := end >= len(plaintext)
		recordPlain := append([]byte{}, plaintext[off:end]...)
		if last {
			recordPlain = append(recordPlain, 0x02)
		} else {
			recordPlain = append(recordPlain, 0x01)
		}
		encrypted = append(encrypted, aead.Seal(nil, webPushIV(nonce, counter), recordPlain, nil)...)
		counter++
	}

	body := make([]byte, 0, 16+4+1+len(senderPub)+len(encrypted))
	body = append(body, salt...)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], webPushRecordSize)
	body = append(body, rs[:]...)
	body = append(body, byte(len(senderPub)))
	body = append(body, senderPub...)
	body = append(body, encrypted...)
	return body, nil
}

func signVAPIDJWT(priv *ecdsa.PrivateKey, aud string, sub string, exp time.Time) (string, error) {
	header := b64URLNoPad([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims := map[string]any{
		"aud": aud,
		"exp": exp.Unix(),
		"sub": sub,
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	token := header + "." + b64URLNoPad(claimsJSON)
	sum := sha256.Sum256([]byte(token))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := append(leftPad(r.Bytes(), 32), leftPad(s.Bytes(), 32)...)
	return token + "." + b64URLNoPad(sig), nil
}

func decodeP256Public(v string) ([]byte, *big.Int, *big.Int, error) {
	raw, err := b64URLDecode(v)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode p256dh: %w", err)
	}
	if len(raw) != 65 || raw[0] != 0x04 {
		return nil, nil, nil, errors.New("p256dh must be an uncompressed P-256 point")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), raw)
	if x == nil || y == nil {
		return nil, nil, nil, errors.New("p256dh is not on P-256")
	}
	return raw, x, y, nil
}

func b64URLDecode(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	if b, err := base64.RawURLEncoding.DecodeString(v); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(v); err == nil {
		return b, nil
	}
	return nil, errors.New("invalid base64url")
}

func hkdfExtract(salt []byte, ikm []byte) []byte {
	h := hmac.New(sha256.New, salt)
	h.Write(ikm)
	return h.Sum(nil)
}

func hkdfExpand(prk []byte, info []byte, length int) []byte {
	var out []byte
	var prev []byte
	counter := byte(1)
	for len(out) < length {
		h := hmac.New(sha256.New, prk)
		h.Write(prev)
		h.Write(info)
		h.Write([]byte{counter})
		prev = h.Sum(nil)
		out = append(out, prev...)
		counter++
	}
	return out[:length]
}

func webPushIV(base []byte, counter uint64) []byte {
	iv := append([]byte{}, base...)
	mask := binary.BigEndian.Uint64(iv[4:])
	binary.BigEndian.PutUint64(iv[4:], counter^mask)
	return iv
}

func leftPad(v []byte, n int) []byte {
	if len(v) >= n {
		return v
	}
	out := make([]byte, n)
	copy(out[n-len(v):], v)
	return out
}
