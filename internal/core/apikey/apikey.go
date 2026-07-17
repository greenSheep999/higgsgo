// Package apikey generates and hashes higgsgo API keys.
//
// Format: sk-hg-<40 hex chars>
//   - sk = "secret key" (OpenAI convention)
//   - hg = "higgsgo" scope
//   - 20 random bytes → 40 hex chars
//
// Only the SHA-256 of the plaintext is persisted. We use SHA-256 instead of
// bcrypt because API-key auth happens on every /v1 request and must be cheap
// (bcrypt would add ~100ms per call). The key is high-entropy (160 bits),
// so a fast hash is safe here — bcrypt's slow-hash defense is aimed at
// low-entropy passwords.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Prefix is the fixed part of every higgsgo key.
const Prefix = "sk-hg-"

// ErrInvalidKey is returned when a supplied key does not parse.
var ErrInvalidKey = errors.New("api key: invalid format")

// Generate returns a fresh (plaintext, hash) pair. The plaintext is shown
// to the user exactly once; only the hash is stored.
func Generate() (plaintext, hash string, err error) {
	var buf [20]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	plaintext = Prefix + hex.EncodeToString(buf[:])
	hash = Hash(plaintext)
	return plaintext, hash, nil
}

// Hash returns the SHA-256 hex digest of the plaintext key.
// The digest is what we compare against the api_keys.key_hash column.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Parse validates a key's shape and returns its hash. Rejects anything that
// isn't Prefix + 40 hex chars.
func Parse(plaintext string) (hash string, err error) {
	if !strings.HasPrefix(plaintext, Prefix) {
		return "", ErrInvalidKey
	}
	body := plaintext[len(Prefix):]
	if len(body) != 40 {
		return "", ErrInvalidKey
	}
	if _, err := hex.DecodeString(body); err != nil {
		return "", ErrInvalidKey
	}
	return Hash(plaintext), nil
}
