// Package hash provides the at-rest hasher for credentials.
package hash

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/marcustut/herald/internal/app"
)

// SHA256 hashes credentials at rest. Tokens are 256-bit random values, so a
// fast one-way hash is sufficient; a slow KDF would only add latency to every
// authentication without adding real work factor against a random secret.
type SHA256 struct{}

func (SHA256) Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

var _ app.Hasher = SHA256{}
