package domain

import (
	"fmt"
	"strings"
	"time"
)

const (
	MaxTitleLen = 200
	MaxBodyLen  = 64 * 1024
	MaxLinks    = 20
)

// Report is the aggregate root: an accepted agent report, equivalent to the
// "event" in the queue model. It is immutable once constructed; the persisted
// row is the audit record. Body is untrusted free text and is stored as-is;
// Redact produces the safe copy used for storage/rendering.
type Report struct {
	ID             ReportID
	TenantID       TenantID
	ProjectID      ProjectID
	AgentID        AgentID
	Status         Status
	Title          string
	Body           string
	Links          []Link
	Context        Context
	IdempotencyKey IdempotencyKey
	CreatedAt      time.Time
}

// NewReportInput carries everything needed to construct a Report. IDs and the
// clock are supplied by the caller (application layer), keeping the domain free
// of infrastructure concerns.
type NewReportInput struct {
	ID             ReportID
	TenantID       TenantID
	ProjectID      ProjectID
	AgentID        AgentID
	Status         Status
	Title          string
	Body           string
	Links          []Link
	Context        Context
	IdempotencyKey IdempotencyKey
	CreatedAt      time.Time
}

func NewReport(in NewReportInput) (Report, error) {
	switch {
	case !validID(string(in.ID)):
		return Report{}, fmt.Errorf("%w: report id", ErrInvalidReport)
	case !validID(string(in.TenantID)):
		return Report{}, fmt.Errorf("%w: tenant id", ErrInvalidReport)
	case !validSlug(string(in.ProjectID)):
		return Report{}, fmt.Errorf("%w: project id", ErrInvalidReport)
	}
	if !in.Status.Valid() {
		return Report{}, fmt.Errorf("%w: %q", ErrInvalidStatus, in.Status)
	}
	if !in.AgentID.Valid() {
		return Report{}, fmt.Errorf("%w: %q", ErrInvalidAgent, in.AgentID)
	}
	ctx, err := NewContext(in.Context)
	if err != nil {
		return Report{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Report{}, ErrTitleRequired
	}
	if len(title) > MaxTitleLen {
		return Report{}, fmt.Errorf("%w: %d > %d", ErrTitleTooLong, len(title), MaxTitleLen)
	}
	if len(in.Body) > MaxBodyLen {
		return Report{}, fmt.Errorf("%w: %d > %d", ErrBodyTooLong, len(in.Body), MaxBodyLen)
	}
	if len(in.Links) > MaxLinks {
		return Report{}, fmt.Errorf("%w: %d > %d", ErrTooManyLinks, len(in.Links), MaxLinks)
	}
	if strings.TrimSpace(string(in.IdempotencyKey)) == "" {
		return Report{}, fmt.Errorf("%w: empty", ErrInvalidIdempotencyKey)
	}
	if in.CreatedAt.IsZero() {
		return Report{}, fmt.Errorf("%w: created at", ErrInvalidReport)
	}
	links := append([]Link(nil), in.Links...)
	return Report{
		ID:             in.ID,
		TenantID:       in.TenantID,
		ProjectID:      in.ProjectID,
		AgentID:        in.AgentID,
		Status:         in.Status,
		Title:          title,
		Body:           in.Body,
		Links:          links,
		Context:        ctx,
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      in.CreatedAt,
	}, nil
}

// Redact returns a copy with secret-looking strings stripped from the
// free-text fields. The receiver is unchanged.
func (r Report) Redact() Report {
	out := r
	out.Title = Redact(out.Title)
	out.Body = Redact(out.Body)
	out.Links = make([]Link, len(r.Links))
	copy(out.Links, r.Links)
	for i := range out.Links {
		out.Links[i].URL = Redact(out.Links[i].URL)
	}
	if len(out.Context) > 0 {
		ctx := make(Context, len(out.Context))
		for k, v := range out.Context {
			ctx[k] = Redact(v)
		}
		out.Context = ctx
	}
	return out
}
