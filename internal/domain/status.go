package domain

import (
	"fmt"
	"strings"
)

// Status is the outcome an agent events. It is a closed set; parsing is
// case-insensitive but the stored value is always canonical.
type Status string

const (
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
	StatusBlocked Status = "blocked"
)

var validStatus = map[Status]struct{}{
	StatusSuccess: {},
	StatusFailure: {},
	StatusBlocked: {},
}

// ParseStatus validates raw and returns the canonical Status.
func ParseStatus(raw string) (Status, error) {
	s := Status(strings.ToLower(strings.TrimSpace(raw)))
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidStatus, raw)
	}
	return s, nil
}

// Valid events whether s is one of the three known statuses.
func (s Status) Valid() bool {
	_, ok := validStatus[s]
	return ok
}

func (s Status) String() string { return string(s) }
