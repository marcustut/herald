package domain

import (
	"fmt"
	"strings"
)

const (
	MaxIdempotencyKeyLen = 128

	occurrenceKeyPrefix = "occ_"

	// MaxOccurrenceIDLen leaves room for the idempotency-key prefix.
	MaxOccurrenceIDLen = MaxIdempotencyKeyLen - len(occurrenceKeyPrefix)
)

// IdempotencyKey lets a producer retry an accepted request without creating a
// duplicate event. The allowed alphabet is intentionally narrow so the key is
// safe to place in a URL, header and log line.
type IdempotencyKey string

func ParseIdempotencyKey(raw string) (IdempotencyKey, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > MaxIdempotencyKeyLen {
		return "", fmt.Errorf("%w: length", ErrInvalidIdempotencyKey)
	}
	if !validKeyChars(s) {
		return "", fmt.Errorf("%w: disallowed char", ErrInvalidIdempotencyKey)
	}
	return IdempotencyKey(s), nil
}

// OccurrenceID is a caller-supplied stable identifier for one logical event.
// Resending an occurrence is a retry; two genuinely distinct events use
// different ids (or none). The alphabet matches an idempotency key.
type OccurrenceID string

func ParseOccurrenceID(raw string) (OccurrenceID, error) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > MaxOccurrenceIDLen {
		return "", fmt.Errorf("%w: length", ErrInvalidOccurrenceID)
	}
	if !validKeyChars(s) {
		return "", fmt.Errorf("%w: disallowed char", ErrInvalidOccurrenceID)
	}
	return OccurrenceID(s), nil
}

// OccurrenceKey maps an occurrence to the idempotency key that deduplicates its
// retries. The prefix keeps it from colliding with a raw idempotency key.
func OccurrenceKey(id OccurrenceID) IdempotencyKey {
	return IdempotencyKey(occurrenceKeyPrefix + string(id))
}

func validKeyChars(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}
