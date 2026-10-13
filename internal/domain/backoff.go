package domain

import "time"

// Backoff is a capped exponential delay policy. It is deterministic; jitter is
// added by the dispatcher at the infrastructure edge, keeping this pure and
// reproducible in tests.
type Backoff struct {
	Base time.Duration
	Cap  time.Duration
}

// Delay returns Base * 2^(attempt-1), clamped to Cap. attempt is 1-based; the
// first failure waits Base.
func (b Backoff) Delay(attempt int) time.Duration {
	if b.Base <= 0 {
		return 0
	}
	cap := b.Cap
	if cap <= 0 {
		cap = b.Base
	}
	if attempt < 1 {
		attempt = 1
	}
	d := b.Base
	for i := 1; i < attempt; i++ {
		if d >= cap {
			return cap
		}
		d *= 2
	}
	if d > cap {
		return cap
	}
	return d
}
