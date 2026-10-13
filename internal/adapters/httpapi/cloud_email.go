package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/marcustut/herald/internal/app"
)

// emailEventMaxBody bounds the provider webhook body. Provider notifications
// are a few kilobytes; a larger body is refused before it is read.
const emailEventMaxBody = 64 << 10

// maxEventRecipients bounds how many addresses one notification may name, so a
// crafted body cannot make the server ingest an unbounded list in one request.
const maxEventRecipients = 100

// registerEmailEvents mounts the provider bounce/complaint ingestion endpoint.
// It is registered only when the use case and a shared secret are both
// configured, so a self-host or unconfigured deployment exposes no route.
func (c *Cloud) registerEmailEvents(mux *http.ServeMux) {
	if c.EmailEvents == nil || c.BounceSecret == "" {
		return
	}
	mux.HandleFunc("POST /cloud/v1/email/events", c.handleEmailEvent)
}

// handleEmailEvent ingests one provider notification: a delivery acceptance, a
// bounce or a complaint. A permanent bounce or a complaint suppresses the
// address. Ingestion is idempotent by provider event id (see app.EmailEvents),
// so a replayed webhook is a no-op. The request body and the recipient address
// are never logged or echoed.
func (c *Cloud) handleEmailEvent(w http.ResponseWriter, r *http.Request) {
	if !c.authorizeProvider(r) {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid webhook secret", "")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, emailEventMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read request body", "")
		return
	}
	events, err := parseProviderEvents(body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "unrecognized provider event", "")
		return
	}
	accepted := 0
	for _, ev := range events {
		recorded, err := c.EmailEvents.Ingest(r.Context(), ev)
		if err != nil {
			// A malformed recipient is skipped, not fatal: the rest of the
			// batch is still applied. Any other error is a store failure.
			if errors.Is(err, app.ErrInvalidEmailEvent) {
				continue
			}
			c.serverError(w, err)
			return
		}
		if recorded {
			accepted++
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"accepted": accepted})
}

// authorizeProvider checks the shared secret in constant time. The secret is
// the only authentication; the endpoint accepts no other credential.
func (c *Cloud) authorizeProvider(r *http.Request) bool {
	got, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || c.BounceSecret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(c.BounceSecret)) == 1
}

// parseProviderEvents accepts the generic shape and the SES-via-SNS shape. An
// SNS subscription confirmation is acknowledged without acting on it: Herald
// never fetches the SubscribeURL (that would be an unauthenticated outbound
// request), so the owner confirms the subscription in the SNS console. See
// docs/deliverability.md.
func parseProviderEvents(body []byte) ([]app.ProviderEvent, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if raw, ok := envelope["Type"]; ok {
		var typ string
		if err := json.Unmarshal(raw, &typ); err != nil {
			return nil, err
		}
		switch typ {
		case "SubscriptionConfirmation", "UnsubscribeConfirmation":
			return nil, nil
		case "Notification":
			return unwrapAndParse(envelope)
		}
	}
	return parseNotification(body)
}

// unwrapAndParse decodes the JSON string an SNS envelope carries in Message and
// parses the notification it holds.
func unwrapAndParse(envelope map[string]json.RawMessage) ([]app.ProviderEvent, error) {
	raw, ok := envelope["Message"]
	if !ok {
		return nil, errors.New("notification without a message")
	}
	var msg string
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return parseNotification([]byte(msg))
}

// parseNotification reads either an SES notification (identified by
// notificationType) or the generic shape (identified by type).
func parseNotification(body []byte) ([]app.ProviderEvent, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["notificationType"]; ok {
		return parseSESNotification(body)
	}
	return parseGenericEvent(body)
}

type sesRecipient struct {
	EmailAddress string `json:"emailAddress"`
}

type sesNotification struct {
	NotificationType string `json:"notificationType"`
	Bounce           struct {
		BounceType        string         `json:"bounceType"`
		BouncedRecipients []sesRecipient `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint struct {
		ComplainedRecipients []sesRecipient `json:"complainedRecipients"`
	} `json:"complaint"`
	Delivery struct {
		Recipients []string `json:"recipients"`
	} `json:"delivery"`
}

func parseSESNotification(body []byte) ([]app.ProviderEvent, error) {
	var n sesNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, err
	}
	kind, ok := parseEventKind(n.NotificationType)
	if !ok {
		return nil, errors.New("unknown notification type")
	}
	var events []app.ProviderEvent
	switch kind {
	case app.EmailEventBounce:
		// Only a Permanent bounce suppresses; a transient one (full mailbox,
		// greylisting) is recorded but leaves delivery retryable.
		permanent := strings.EqualFold(n.Bounce.BounceType, "Permanent")
		for _, r := range n.Bounce.BouncedRecipients {
			events = append(events, app.ProviderEvent{Kind: kind, Address: r.EmailAddress, Permanent: permanent})
		}
	case app.EmailEventComplaint:
		for _, r := range n.Complaint.ComplainedRecipients {
			events = append(events, app.ProviderEvent{Kind: kind, Address: r.EmailAddress})
		}
	case app.EmailEventDelivery:
		for _, addr := range n.Delivery.Recipients {
			events = append(events, app.ProviderEvent{Kind: kind, Address: addr})
		}
	}
	return capRecipients(events)
}

// genericEvent is the provider-agnostic shape, so any relay can be wired
// without a second SDK.
type genericEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Recipient string `json:"recipient"`
	Address   string `json:"address"`
	Permanent *bool  `json:"permanent"`
}

func parseGenericEvent(body []byte) ([]app.ProviderEvent, error) {
	var g genericEvent
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, err
	}
	kind, ok := parseEventKind(g.Type)
	if !ok {
		return nil, errors.New("unknown event type")
	}
	address := g.Recipient
	if address == "" {
		address = g.Address
	}
	if address == "" {
		return nil, errors.New("event without a recipient")
	}
	permanent := g.Permanent != nil && *g.Permanent
	return []app.ProviderEvent{{ID: g.ID, Kind: kind, Address: address, Permanent: permanent}}, nil
}

func parseEventKind(s string) (app.EmailEventKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "delivery", "delivered", "accept", "accepted":
		return app.EmailEventDelivery, true
	case "bounce", "bounced":
		return app.EmailEventBounce, true
	case "complaint", "complained", "spam":
		return app.EmailEventComplaint, true
	default:
		return "", false
	}
}

// capRecipients rejects a notification that names more recipients than the
// bound, rather than truncating it silently.
func capRecipients(events []app.ProviderEvent) ([]app.ProviderEvent, error) {
	if len(events) > maxEventRecipients {
		return nil, errors.New("too many recipients")
	}
	return events, nil
}
