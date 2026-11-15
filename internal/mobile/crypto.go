// Package mobile implements the hub-owned Mobile feature's wire cipher:
// an HMAC-SHA256 stream cipher with encrypt-then-MAC, using only the Go
// standard library. The key is SHA-256(token UTF-8), where token is the
// hub-generated mobile token (settings key "mobile_token"). The same
// construction is used by the Android app (Kotlin) and the v1 Python
// gateway. See PROTOCOL-v2.md for the full contract and test vector.
//
// Wire format: base64(nonce [16] || ciphertext || tag [32]).
// Keystream block i (from 0) = HMAC-SHA256(key, nonce || counter_be32(i)).
// Tag = HMAC-SHA256(key, nonce || ciphertext), verified (constant-time)
// BEFORE decrypting.
package mobile

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

// KeyFromToken derives the 32-byte cipher key from a mobile token.
func KeyFromToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// keystream returns the first n bytes of the HMAC keystream for nonce.
func keystream(key, nonce []byte, n int) []byte {
	out := make([]byte, 0, n)
	var ctr [4]byte
	for i := uint32(0); len(out) < n; i++ {
		mac := hmac.New(sha256.New, key)
		mac.Write(nonce)
		binary.BigEndian.PutUint32(ctr[:], i)
		mac.Write(ctr[:])
		out = append(out, mac.Sum(nil)...)
	}
	return out[:n]
}

func xorBytes(dst, a, b []byte) {
	for i := range a {
		dst[i] = a[i] ^ b[i]
	}
}

// Encrypt encrypts plaintext with the token-derived key and a fresh random
// 16-byte nonce. It returns base64(nonce || ciphertext || tag).
func Encrypt(token string, plaintext []byte) (string, error) {
	key := KeyFromToken(token)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ks := keystream(key, nonce, len(plaintext))
	ct := make([]byte, len(plaintext))
	xorBytes(ct, plaintext, ks)
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce)
	mac.Write(ct)
	tag := mac.Sum(nil)
	wire := make([]byte, 0, 16+len(ct)+32)
	wire = append(wire, nonce...)
	wire = append(wire, ct...)
	wire = append(wire, tag...)
	return base64.StdEncoding.EncodeToString(wire), nil
}

// Decrypt verifies the tag (constant-time) and then decrypts a wire
// produced by Encrypt. It returns an error on any tampering or bad input.
func Decrypt(token string, wire string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(wire)
	if err != nil {
		return nil, errors.New("invalid wire encoding")
	}
	if len(raw) < 16+32 {
		return nil, errors.New("wire too short")
	}
	nonce := raw[:16]
	ct := raw[16 : len(raw)-32]
	tag := raw[len(raw)-32:]
	key := KeyFromToken(token)
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce)
	mac.Write(ct)
	expect := mac.Sum(nil)
	if !hmac.Equal(expect, tag) {
		return nil, errors.New("tag verification failed")
	}
	ks := keystream(key, nonce, len(ct))
	pt := make([]byte, len(ct))
	xorBytes(pt, ct, ks)
	return pt, nil
}
