// Package apikey generates and hashes higgsgo API keys.
//
// Two plaintext formats coexist so callers and operators can tell an
// operator-facing "default" (admin console) key apart from a normal
// downstream "project" key at a glance:
//
//	sk-hg-<40 hex chars>   — project keys (broadly-shared downstream)
//	sk-adm-<40 hex chars>  — default keys (operator-only, admin console)
//
//   - sk       = "secret key" (OpenAI convention)
//   - hg / adm = higgsgo scope tag; the segment is what visually
//     distinguishes the two kinds in logs and shoulder-surf glimpses
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

// Prefix is the default project-key prefix. Kept as a constant for
// call sites that don't yet care about the kind split (e.g. import
// paths from before the default/project distinction landed).
const Prefix = "sk-hg-"

// ProjectPrefix / AdminPrefix are the two live prefixes. Parse
// accepts either; the auth middleware doesn't need to know which
// kind a key is — that's a metadata concern, not an auth one.
const (
	ProjectPrefix = "sk-hg-"
	AdminPrefix   = "sk-adm-"
)

// Kind classifies which prefix to mint. Kept string-typed so callers
// in the admin handler can pass the domain enum directly (values line
// up: "default" | "project").
type Kind string

const (
	KindProject Kind = "project"
	KindDefault Kind = "default"
)

// ErrInvalidKey is returned when a supplied key does not parse.
var ErrInvalidKey = errors.New("api key: invalid format")

// Generate returns a fresh (plaintext, hash) pair. The plaintext is
// shown to the user exactly once; only the hash is stored. `kind`
// picks the prefix — see the package doc for the format table.
func Generate(kind Kind) (plaintext, hash string, err error) {
	var buf [20]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	prefix := ProjectPrefix
	if kind == KindDefault {
		prefix = AdminPrefix
	}
	plaintext = prefix + hex.EncodeToString(buf[:])
	hash = Hash(plaintext)
	return plaintext, hash, nil
}

// GenerateProject preserves the pre-split call site — mints a
// project (sk-hg-…) key. Kept so callers that only want a project
// key don't need to import the Kind enum.
func GenerateProject() (plaintext, hash string, err error) {
	return Generate(KindProject)
}

// Hash returns the SHA-256 hex digest of the plaintext key.
// The digest is what we compare against the api_keys.key_hash column.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Parse validates a key's shape and returns its hash. Accepts both
// ProjectPrefix and AdminPrefix, followed by exactly 40 hex chars.
func Parse(plaintext string) (hash string, err error) {
	var body string
	switch {
	case strings.HasPrefix(plaintext, ProjectPrefix):
		body = plaintext[len(ProjectPrefix):]
	case strings.HasPrefix(plaintext, AdminPrefix):
		body = plaintext[len(AdminPrefix):]
	default:
		return "", ErrInvalidKey
	}
	if len(body) != 40 {
		return "", ErrInvalidKey
	}
	if _, err := hex.DecodeString(body); err != nil {
		return "", ErrInvalidKey
	}
	return Hash(plaintext), nil
}

// Last4 returns the last 4 characters of a plaintext key body,
// useful for masked display in admin lists. Returns "" for keys
// shorter than 4 hex chars past their prefix.
func Last4(plaintext string) string {
	if len(plaintext) < 4 {
		return ""
	}
	return plaintext[len(plaintext)-4:]
}
