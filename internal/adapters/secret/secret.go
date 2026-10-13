// Package secret mints credential secrets from the system CSPRNG.
package secret

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/marcustut/herald/internal/app"
)

const secretBytes = 32

// Generator mints 256-bit secrets. crypto/rand.Read never returns an error on
// supported platforms.
type Generator struct{}

func (Generator) NewSecret() string {
	b := make([]byte, secretBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ app.SecretGenerator = Generator{}
