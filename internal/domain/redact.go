package domain

import "regexp"

// RedactionMarker replaces every secret-looking match.
const RedactionMarker = "[REDACTED]"

type redactRule struct {
	re   *regexp.Regexp
	repl string
}

// Rules are ordered; keyed credential patterns run after token-shaped ones so
// that "token: eyJ..." is redacted once rather than partially. Every rule is
// idempotent: applying Redact twice yields the same string, because no
// replacement re-matches its own pattern.
var redactRules = []redactRule{
	// PEM private key blocks (multi-line).
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), RedactionMarker},
	// Cloud and service provider token shapes.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`), RedactionMarker},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), RedactionMarker},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), RedactionMarker},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`), RedactionMarker},
	// JWTs.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), RedactionMarker},
	// Credentials embedded in a URL: scheme://user:pass@host
	{regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`), "://" + RedactionMarker + "@"},
	// Scheme-prefixed auth values (Bearer/Basic), which the keyed rule below
	// would otherwise truncate to the scheme word alone.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`), `${1} ` + RedactionMarker},
	// key: value / key=value for credential-flavoured keys. Only the value is
	// replaced so a second pass is a no-op.
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|secret[_-]?key|client[_-]?secret|authorization|auth[_-]?token|private[_-]?key)\b(\s*[:=]\s*)(\S+)`), `${1}${2}` + RedactionMarker},
}

// Redact strips secret-looking strings from untrusted text. It errs toward
// over-redaction; a false positive costs a mangled line in a notification,
// a false negative leaks a credential.
func Redact(s string) string {
	for _, r := range redactRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
