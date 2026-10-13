package domain

import (
	"fmt"
	"strings"
)

// AgentID identifies the agent that produced a report. Unlike Title and Body it
// is never supplied by the client: it is resolved from the bearer credential,
// so it is trustworthy provenance an agent cannot spoof.
type AgentID string

func ParseAgentID(raw string) (AgentID, error) {
	raw = strings.TrimSpace(raw)
	if !validSlug(raw) {
		return "", fmt.Errorf("%w: %q", ErrInvalidAgent, raw)
	}
	return AgentID(raw), nil
}

func (a AgentID) Valid() bool    { return validSlug(string(a)) }
func (a AgentID) String() string { return string(a) }
