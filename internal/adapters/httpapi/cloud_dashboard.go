package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Dashboard pagination bounds. A page is small enough for a phone and the
// offset is capped so a caller cannot make the database count forever.
const (
	defaultPageSize = 25
	maxPageSize     = 100
	maxPageOffset   = 100000
)

// registerDashboard wires the read-only dashboard queries. Every route is
// session-scoped: the tenant comes from the authenticated session and is never
// read from the request, so a client-supplied tenant or project id can never
// widen visibility. When the log is not wired the routes are simply absent.
func (c *Cloud) registerDashboard(mux *http.ServeMux) {
	if c.Events == nil {
		return
	}
	mux.HandleFunc("GET /cloud/v1/events", c.handleListEvents)
	mux.HandleFunc("GET /cloud/v1/deliveries", c.handleListDeliveries)
}

// eventLogDTO is one event row. Title is redacted: a failure report can carry a
// credential pasted into the free text, and the dashboard must not echo it back
// as a leak. Body is deliberately omitted — the dashboard renders the title, and
// shipping every body would bloat the page on a phone.
type eventLogDTO struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Agent     string    `json:"agent"`
	Status    string    `json:"status"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
}

func eventLogDTOFrom(e domain.Event) eventLogDTO {
	return eventLogDTO{
		ID:        string(e.ID),
		Project:   string(e.ProjectID),
		Agent:     string(e.AgentID),
		Status:    string(e.Status),
		Title:     domain.Redact(e.Title),
		CreatedAt: e.CreatedAt,
	}
}

// deliveryLogDTO is one delivery attempt. LastError is the actionable
// diagnostic and is redacted, so a token or a credential embedded in a URL that
// appears in a failure message is not exposed.
type deliveryLogDTO struct {
	ID            string    `json:"id"`
	EventID       string    `json:"eventId"`
	Sink          string    `json:"sink"`
	State         string    `json:"state"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
	LastError     string    `json:"lastError,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func deliveryLogDTOFrom(d domain.Delivery) deliveryLogDTO {
	return deliveryLogDTO{
		ID:            string(d.ID),
		EventID:       string(d.EventID),
		Sink:          string(d.SinkID),
		State:         string(d.State),
		Attempts:      d.Attempts,
		NextAttemptAt: d.NextAttemptAt,
		LastError:     domain.Redact(d.LastError),
		UpdatedAt:     d.UpdatedAt,
	}
}

// pageDTO tells the client how to ask for the next page. HasMore is exact: the
// store is asked for one extra row and the surplus is dropped.
type pageDTO struct {
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	HasMore bool `json:"hasMore"`
}

// handleListEvents returns the session tenant's events, newest first.
func (c *Cloud) handleListEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	f, limit, offset, ok := eventFilter(w, r)
	if !ok {
		return
	}
	events, err := c.Events.ListEvents(r.Context(), sess.Identity.TenantID, f, limit+1, offset)
	if err != nil {
		c.serverError(w, err)
		return
	}
	page := pageDTO{Limit: limit, Offset: offset, HasMore: len(events) > limit}
	if page.HasMore {
		events = events[:limit]
	}
	out := make([]eventLogDTO, len(events))
	for i, e := range events {
		out[i] = eventLogDTOFrom(e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "page": page})
}

// handleListDeliveries returns the session tenant's delivery attempts, newest
// first, with the event's project/state/date filters.
func (c *Cloud) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	f, limit, offset, ok := deliveryLogFilter(w, r)
	if !ok {
		return
	}
	rows, err := c.Events.ListDeliveryAttempts(r.Context(), sess.Identity.TenantID, f, limit+1, offset)
	if err != nil {
		c.serverError(w, err)
		return
	}
	page := pageDTO{Limit: limit, Offset: offset, HasMore: len(rows) > limit}
	if page.HasMore {
		rows = rows[:limit]
	}
	out := make([]deliveryLogDTO, len(rows))
	for i, d := range rows {
		out[i] = deliveryLogDTOFrom(d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out, "page": page})
}

// eventFilter reads and validates the event query parameters. It writes the
// error response itself.
func eventFilter(w http.ResponseWriter, r *http.Request) (app.EventFilter, int, int, bool) {
	q := r.URL.Query()
	var f app.EventFilter
	f.Project = domain.ProjectID(q.Get("project"))
	if raw := q.Get("status"); raw != "" {
		status, err := domain.ParseStatus(raw)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", "unknown status", "status")
			return f, 0, 0, false
		}
		f.Status = status
	}
	since, until, ok := timeRange(w, r)
	if !ok {
		return f, 0, 0, false
	}
	f.Since, f.Until = since, until
	limit, offset := pageParams(r)
	return f, limit, offset, true
}

// deliveryLogFilter reads and validates the delivery query parameters.
func deliveryLogFilter(w http.ResponseWriter, r *http.Request) (app.DeliveryLogFilter, int, int, bool) {
	q := r.URL.Query()
	var f app.DeliveryLogFilter
	f.Project = domain.ProjectID(q.Get("project"))
	f.EventID = domain.EventID(q.Get("event"))
	if raw := q.Get("state"); raw != "" {
		if !validDeliveryState(domain.DeliveryState(raw)) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", "unknown delivery state", "state")
			return f, 0, 0, false
		}
		f.State = domain.DeliveryState(raw)
	}
	since, until, ok := timeRange(w, r)
	if !ok {
		return f, 0, 0, false
	}
	f.Since, f.Until = since, until
	limit, offset := pageParams(r)
	return f, limit, offset, true
}

func validDeliveryState(s domain.DeliveryState) bool {
	switch s {
	case domain.DeliveryPending, domain.DeliveryInflight, domain.DeliveryDelivered, domain.DeliveryDead:
		return true
	default:
		return false
	}
}

// timeRange parses since/until. A full RFC3339 timestamp is exact; a bare
// YYYY-MM-DD (what an HTML date input sends) covers the whole day. Until is
// exclusive, so a date's end is the next midnight.
func timeRange(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "since must be RFC3339 or YYYY-MM-DD", "since")
		return time.Time{}, time.Time{}, false
	}
	until, err := parseUntil(r.URL.Query().Get("until"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "until must be RFC3339 or YYYY-MM-DD", "until")
		return time.Time{}, time.Time{}, false
	}
	return since, until, true
}

func parseSince(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", raw)
}

func parseUntil(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	d, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	return d.AddDate(0, 0, 1), nil
}

// pageParams clamps limit and offset so a bad or hostile value cannot ask for
// an unbounded page. A value that does not parse falls back to its default.
func pageParams(r *http.Request) (limit, offset int) {
	q := r.URL.Query()
	return clampInt(q.Get("limit"), defaultPageSize, 1, maxPageSize),
		clampInt(q.Get("offset"), 0, 0, maxPageOffset)
}

func clampInt(raw string, def, min, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
