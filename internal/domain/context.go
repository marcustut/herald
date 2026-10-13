package domain

import "fmt"

const (
	MaxContextEntries  = 8
	MaxContextKeyLen   = 32
	MaxContextValueLen = 128
)

// Context is bounded, untrusted metadata attached to a report — for example the
// host an agent ran on. Keys are validated so the pairs can be rendered and
// logged safely; values are redacted like any other free text. Keys naming
// fields that must originate from the credential are reserved and rejected,
// so a client cannot fake provenance through the body.
type Context map[string]string

var reservedContextKeys = map[string]struct{}{
	"agent":   {},
	"project": {},
	"tenant":  {},
	"status":  {},
}

func NewContext(in map[string]string) (Context, error) {
	if len(in) > MaxContextEntries {
		return nil, fmt.Errorf("%w: %d entries", ErrInvalidContext, len(in))
	}
	out := make(Context, len(in))
	for k, v := range in {
		if err := validateContextEntry(k, v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func validateContextEntry(k, v string) error {
	if len(k) > MaxContextKeyLen || !validContextKey(k) {
		return fmt.Errorf("%w: key %q", ErrInvalidContext, k)
	}
	if _, reserved := reservedContextKeys[k]; reserved {
		return fmt.Errorf("%w: reserved key %q", ErrInvalidContext, k)
	}
	if len(v) > MaxContextValueLen || hasControlChars(v) {
		return fmt.Errorf("%w: value for key %q", ErrInvalidContext, k)
	}
	return nil
}

func validContextKey(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return s != ""
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Host returns the self-asserted host label, if one was supplied.
func (c Context) Host() string { return c["host"] }
