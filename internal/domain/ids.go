package domain

import "strings"

// Typed identifiers. Using distinct types prevents passing a tenant where a
// project is expected at compile time. Construction is deliberately plain
// string conversion; validation happens where external input enters (token
// parsing, HTTP decoding), and NewReport enforces presence on the aggregate.
type (
	ReportID   string
	DeliveryID string
	TenantID   string
	ProjectID  string
	SinkID     string
)

const maxIDLen = 64

// validID reports whether s is a usable identifier: non-blank and bounded.
func validID(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len(s) <= maxIDLen
}
