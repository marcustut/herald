package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/adapters/httpapi"
	"github.com/marcustut/herald/internal/app"
)

// memSuppressions is an in-memory app.SuppressionStore for the black-box HTTP
// tests. It mirrors the SQL store: idempotent by event id, suppression applied
// only on the first sighting of an id.
type memSuppressions struct {
	mu         sync.Mutex
	suppressed map[string]string
	seen       map[string]bool
}

func newMemSuppressions() *memSuppressions {
	return &memSuppressions{suppressed: map[string]string{}, seen: map[string]bool{}}
}

func (s *memSuppressions) Suppressed(_ context.Context, address string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.suppressed[address]
	return ok, nil
}

func (s *memSuppressions) IngestProviderEvent(_ context.Context, e app.ProviderEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[e.ID] {
		return false, nil
	}
	s.seen[e.ID] = true
	if e.Suppresses() {
		s.suppressed[e.Address] = e.Reason()
	}
	return true, nil
}

func (s *memSuppressions) isSuppressed(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.suppressed[address]
	return ok
}

func (s *memSuppressions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suppressed)
}

func newEmailEventServer(t *testing.T, secret string) (*httptest.Server, *memSuppressions) {
	t.Helper()
	store := newMemSuppressions()
	api := &httpapi.Server{Cloud: &httpapi.Cloud{
		EmailEvents:  &app.EmailEvents{Suppressions: store, Clock: cloudClock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}},
		BounceSecret: secret,
	}}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func postEmailEvent(t *testing.T, ts *httptest.Server, secret, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/email/events", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeAccepted(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out.Accepted, string(raw)
}

func TestEmailEventRouteAbsentWithoutUseCaseOrSecret(t *testing.T) {
	store := newMemSuppressions()
	for _, tc := range []struct {
		name  string
		cloud *httpapi.Cloud
	}{
		{"no use case", &httpapi.Cloud{BounceSecret: "s"}},
		{"no secret", &httpapi.Cloud{EmailEvents: &app.EmailEvents{Suppressions: store, Clock: cloudClock{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer((&httpapi.Server{Cloud: tc.cloud}).Handler())
			defer ts.Close()
			resp := postEmailEvent(t, ts, "s", `{"id":"e1","type":"bounce","recipient":"a@b.example","permanent":true}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

func TestEmailEventRequiresSecret(t *testing.T) {
	ts, store := newEmailEventServer(t, "s3cret")
	body := `{"id":"e1","type":"complaint","recipient":"a@b.example"}`
	for _, secret := range []string{"", "wrong"} {
		resp := postEmailEvent(t, ts, secret, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("secret %q status = %d, want 401", secret, resp.StatusCode)
		}
	}
	if store.count() != 0 {
		t.Fatal("an unauthenticated request changed state")
	}
}

func TestEmailEventPermanentBounceSuppresses(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	resp := postEmailEvent(t, ts, "s", `{"id":"e1","type":"bounce","recipient":"Ops@Example.COM","permanent":true}`)
	accepted, raw := decodeAccepted(t, resp)
	if resp.StatusCode != http.StatusAccepted || accepted != 1 {
		t.Fatalf("status=%d accepted=%d", resp.StatusCode, accepted)
	}
	if !store.isSuppressed("Ops@example.com") {
		t.Fatalf("address not suppressed; store = %v", store.suppressed)
	}
	if strings.Contains(raw, "Ops@Example.COM") {
		t.Fatalf("response echoed the address: %s", raw)
	}
}

func TestEmailEventReplayIsIdempotent(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	body := `{"id":"evt-9","type":"complaint","recipient":"a@b.example"}`

	first := postEmailEvent(t, ts, "s", body)
	accepted, _ := decodeAccepted(t, first)
	if accepted != 1 {
		t.Fatalf("first accepted = %d, want 1", accepted)
	}
	second := postEmailEvent(t, ts, "s", body)
	accepted, _ = decodeAccepted(t, second)
	if accepted != 0 {
		t.Fatalf("replay accepted = %d, want 0", accepted)
	}
	if store.count() != 1 {
		t.Fatalf("suppressions = %d, want 1", store.count())
	}
}

// A transient bounce is accepted (recorded) but must not suppress the address.
func TestEmailEventTransientBounceDoesNotSuppress(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	resp := postEmailEvent(t, ts, "s", `{"id":"e2","type":"bounce","recipient":"a@b.example","permanent":false}`)
	accepted, _ := decodeAccepted(t, resp)
	if accepted != 1 {
		t.Fatalf("accepted = %d, want 1", accepted)
	}
	if store.isSuppressed("a@b.example") {
		t.Fatal("transient bounce suppressed the address")
	}
}

func TestEmailEventComplaintSuppresses(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	resp := postEmailEvent(t, ts, "s", `{"id":"e3","type":"complaint","recipient":"a@b.example"}`)
	if accepted, _ := decodeAccepted(t, resp); accepted != 1 {
		t.Fatalf("accepted = %d, want 1", accepted)
	}
	if !store.isSuppressed("a@b.example") {
		t.Fatal("complaint did not suppress the address")
	}
}

// The SES-via-SNS shape: the notification is a JSON string inside Message.
func TestEmailEventSESSNSEnvelope(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	body := `{"Type":"Notification","MessageId":"sns-1",` +
		`"Message":"{\"notificationType\":\"Bounce\",\"bounce\":{\"bounceType\":\"Permanent\",\"bouncedRecipients\":[{\"emailAddress\":\"gone@example.com\"}]}}"}`

	resp := postEmailEvent(t, ts, "s", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !store.isSuppressed("gone@example.com") {
		t.Fatal("SES permanent bounce did not suppress")
	}
}

// A transient SES bounce (bounceType Transient) must not suppress.
func TestEmailEventSESSNSTransientBounce(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	body := `{"Type":"Notification",` +
		`"Message":"{\"notificationType\":\"Bounce\",\"bounce\":{\"bounceType\":\"Transient\",\"bouncedRecipients\":[{\"emailAddress\":\"full@example.com\"}]}}"}`

	resp := postEmailEvent(t, ts, "s", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if store.isSuppressed("full@example.com") {
		t.Fatal("transient SES bounce suppressed the address")
	}
}

// An SNS subscription confirmation is acknowledged without acting, and without
// fetching the SubscribeURL (which would be an unauthenticated outbound call).
func TestEmailEventSubscriptionConfirmationIsAcknowledged(t *testing.T) {
	ts, store := newEmailEventServer(t, "s")
	body := `{"Type":"SubscriptionConfirmation","SubscribeURL":"http://169.254.169.254/latest/meta-data/"}`
	resp := postEmailEvent(t, ts, "s", body)
	accepted, _ := decodeAccepted(t, resp)
	if resp.StatusCode != http.StatusAccepted || accepted != 0 {
		t.Fatalf("status=%d accepted=%d", resp.StatusCode, accepted)
	}
	if store.count() != 0 {
		t.Fatal("subscription confirmation changed state")
	}
}

func TestEmailEventMalformed(t *testing.T) {
	ts, _ := newEmailEventServer(t, "s")
	for _, body := range []string{
		`{`,
		`{"id":"e1","type":"pigeon","recipient":"a@b.example"}`,
		`{"id":"e1","type":"bounce"}`,
	} {
		resp := postEmailEvent(t, ts, "s", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("body %q status = %d, want 422", body, resp.StatusCode)
		}
	}
}
