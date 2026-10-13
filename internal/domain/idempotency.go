package domain

import (
	"fmt"
	"strings"
)

const MaxIdempotencyKeyLen = 128

// IdempotencyKey lets a producer retry an accepted request without creating a
// duplicate report. The allowed alphabet is intentionally narrow so the key is
// safe to place in a URL, header and log line.
type IdempotencyKey string

func ParseIdempotencyKey(raw string) (IdempotencyKey, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > MaxIdempotencyKeyLen {
		return "", fmt.Errorf("%w: length", ErrInvalidIdempotencyKey)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return "", fmt.Errorf("%w: disallowed char %q", ErrInvalidIdempotencyKey, r)
		}
	}
	return IdempotencyKey(s), nil
}
