package domain

// NormalizeEmail validates and canonicalizes an email address for identity
// lookup. The local part is preserved byte-for-byte (it may be case-sensitive),
// while the domain is lower-cased, so the same human always maps to one user.
func NormalizeEmail(s string) (string, error) { return validEmail(s) }
