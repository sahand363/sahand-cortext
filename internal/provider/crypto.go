package provider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"strings"
)

// apiKeyEncPrefix marks an encrypted API key value in the settings table.
const apiKeyEncPrefix = "enc:v1:"

// apiKeyEncryptionKey returns the 32-byte key from HUB_KEY_ENCRYPTION_KEY
// (base64). The key lives in the environment file, outside the database, so
// a leaked database alone does not expose the API keys. Returns nil when the
// key is unset or invalid; callers then fall back to plaintext storage.
func apiKeyEncryptionKey() []byte {
	raw := strings.TrimSpace(os.Getenv("HUB_KEY_ENCRYPTION_KEY"))
	if raw == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// APIKeyEncryptionReady reports whether provider API keys can be encrypted.
func APIKeyEncryptionReady() bool {
	return apiKeyEncryptionKey() != nil
}

// EncryptAPIKey encrypts a plaintext API key for storage (AES-256-GCM with a
// random nonce). It returns the stored form ("enc:v1:<base64>") and true, or
// the plaintext unchanged and false when no encryption key is configured.
func EncryptAPIKey(plaintext string) (string, bool) {
	key := apiKeyEncryptionKey()
	if key == nil {
		return plaintext, false
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return plaintext, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext, false
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return plaintext, false
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return apiKeyEncPrefix + base64.StdEncoding.EncodeToString(out), true
}

// DecryptAPIKey returns the plaintext for a stored API key value. Values
// without the "enc:v1:" prefix (legacy plaintext) are returned unchanged.
// When the encryption key is missing or decryption fails, the stored value
// is returned unchanged so the caller can surface the problem instead of
// crashing.
func DecryptAPIKey(stored string) string {
	if !strings.HasPrefix(stored, apiKeyEncPrefix) {
		return stored
	}
	key := apiKeyEncryptionKey()
	if key == nil {
		return stored
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, apiKeyEncPrefix))
	if err != nil {
		return stored
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return stored
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return stored
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return stored
	}
	pt, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return stored
	}
	return string(pt)
}

// isEncryptedAPIKeyValue reports whether a stored value is encrypted.
func isEncryptedAPIKeyValue(v string) bool {
	return strings.HasPrefix(v, apiKeyEncPrefix)
}
