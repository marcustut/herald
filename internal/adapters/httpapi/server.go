// Package httpapi exposes the versioned HTTP contract that other projects call.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

const defaultMaxBody = 256 << 10

// Server wires the HTTP surface to the application use cases.
type Server struct {
	Auth    app.Authenticator
	Submit  app.SubmitEvent
	Limiter app.RateLimiter
	Ready   func(ctx context.Context) error
	MaxBody int64
	Logger  *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handleSubmit)
	// Deprecated alias for the pre-Event contract; behaves identically.
	mux.HandleFunc("POST /v1/reports", s.handleSubmit)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	return mux
}

type linkDTO struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type submitPayload struct {
	Project        string            `json:"project"`
	Status         string            `json:"status"`
	Title          string            `json:"title"`
	Body           string            `json:"body"`
	Links          []linkDTO         `json:"links"`
	Context        map[string]string `json:"context"`
	Agent          string            `json:"agent"`
	OccurrenceID   string            `json:"occurrenceId"`
	IdempotencyKey string            `json:"idempotencyKey"`
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/reports" {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Link", `</v1/events>; rel="successor-version"`)
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", "")
		return
	}

	max := s.MaxBody
	if max <= 0 {
		max = defaultMaxBody
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large", "")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read request body", "")
		return
	}

	bearer, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing bearer token", "")
		return
	}

	var payload submitPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "malformed JSON", "")
		return
	}

	principal, err := s.Auth.Authenticate(r.Context(), bearer, domain.ProjectID(payload.Project))
	if err != nil {
		if errors.Is(err, app.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or out-of-scope token", "")
			return
		}
		s.serverError(w, err)
		return
	}

	// Rate limiting happens after authentication so an invalid token always
	// answers 401 and a caller cannot use 429 to probe for valid tokens.
	if s.Limiter != nil {
		decision, err := s.Limiter.Allow(r.Context(), principal.TenantID, principal.ProjectID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		writeRateLimit(w, decision)
		if !decision.Allowed {
			w.Header().Set("Retry-After", seconds(decision.RetryAfter))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "submit limit exceeded", "")
			return
		}
	}

	id, err := s.Submit.Handle(r.Context(), principal, payload.request())
	if err != nil {
		if fieldFor(err) != "" || isValidation(err) {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error(), fieldFor(err))
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": string(id)})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.Ready != nil {
		if err := s.Ready(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "not ready", "")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ready\n")
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	if s.Logger != nil {
		s.Logger.Error("request failed", "error", err)
	}
	writeError(w, http.StatusServiceUnavailable, "unavailable", "service unavailable", "")
}

func (p submitPayload) request() app.SubmitRequest {
	links := make([]app.LinkInput, len(p.Links))
	for i, l := range p.Links {
		links[i] = app.LinkInput{Label: l.Label, URL: l.URL}
	}
	return app.SubmitRequest{
		Status:         p.Status,
		Title:          p.Title,
		Body:           p.Body,
		Links:          links,
		Context:        p.Context,
		Agent:          p.Agent,
		OccurrenceID:   p.OccurrenceID,
		IdempotencyKey: p.IdempotencyKey,
	}
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func isJSON(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

func isValidation(err error) bool {
	for _, sentinel := range []error{
		domain.ErrInvalidEvent,
		domain.ErrInvalidStatus,
		domain.ErrInvalidLink,
		domain.ErrInvalidAgent,
		domain.ErrInvalidContext,
		domain.ErrInvalidIdempotencyKey,
		domain.ErrInvalidOccurrenceID,
		domain.ErrTitleRequired,
		domain.ErrTitleTooLong,
		domain.ErrBodyTooLong,
		domain.ErrTooManyLinks,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

func fieldFor(err error) string {
	switch {
	case errors.Is(err, domain.ErrTitleRequired), errors.Is(err, domain.ErrTitleTooLong):
		return "title"
	case errors.Is(err, domain.ErrBodyTooLong):
		return "body"
	case errors.Is(err, domain.ErrInvalidStatus):
		return "status"
	case errors.Is(err, domain.ErrInvalidLink), errors.Is(err, domain.ErrTooManyLinks):
		return "links"
	case errors.Is(err, domain.ErrInvalidAgent):
		return "agent"
	case errors.Is(err, domain.ErrInvalidContext):
		return "context"
	case errors.Is(err, domain.ErrInvalidIdempotencyKey):
		return "idempotencyKey"
	case errors.Is(err, domain.ErrInvalidOccurrenceID):
		return "occurrenceId"
	default:
		return ""
	}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message, field string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Field: field}})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeRateLimit advertises the tightest remaining budget. Reset is Unix
// seconds; a decision with no active limit emits no headers.
func writeRateLimit(w http.ResponseWriter, d app.Decision) {
	if d.Limit <= 0 {
		return
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
	if !d.Reset.IsZero() {
		h.Set("X-RateLimit-Reset", strconv.FormatInt(d.Reset.Unix(), 10))
	}
}

// seconds renders a Retry-After delta as whole seconds, never below one.
func seconds(d time.Duration) string {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}
