package sink

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/config"
	"github.com/marcustut/herald/internal/domain"
)

// stubDestinations is a minimal app.DestinationStore whose reads are driven by
// a fixed map, so the source can be exercised without a database.
type stubDestinations struct {
	byID map[domain.DestinationID]domain.Destination
	err  error
}

func (s *stubDestinations) PutDestination(context.Context, domain.Destination) error { return nil }

func (s *stubDestinations) Destination(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) (domain.Destination, bool, error) {
	if s.err != nil {
		return domain.Destination{}, false, s.err
	}
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return domain.Destination{}, false, nil
	}
	return d, true, nil
}

func (s *stubDestinations) DestinationsByProject(context.Context, domain.TenantID, domain.ProjectID) ([]domain.Destination, error) {
	return nil, nil
}

func (s *stubDestinations) DeleteDestination(context.Context, domain.TenantID, domain.ProjectID, domain.DestinationID) error {
	return app.ErrDestinationNotFound
}

func (s *stubDestinations) MarkVerified(context.Context, domain.TenantID, domain.ProjectID, domain.DestinationID, time.Time) error {
	return nil
}

func destination(id domain.DestinationID, kind domain.DestinationKind, target string, enabled bool, verifiedAt time.Time) domain.Destination {
	return domain.Destination{
		ID: id, TenantID: "ten_a", ProjectID: "alpha", Kind: kind, Target: target,
		Enabled: enabled, VerifiedAt: verifiedAt, CreatedAt: verifiedAt,
	}
}

func TestDestinationSourceBuildsEmailForVerifiedTarget(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_e": destination("dst_e", domain.DestinationEmail, "ops@example.com", true, now),
	}}
	cfg := config.Gateway{SMTP: config.SMTP{Addr: "127.0.0.1:587", From: "herald@x", FromName: "Herald"}}
	ft := &fakeTransport{}
	src := NewDestinationSource(store, cfg, "", "").WithMailTransport(ft)

	s, ok, err := src.Sink(context.Background(), "ten_a", "alpha", "dst_e")
	if err != nil || !ok {
		t.Fatalf("Sink ok=%v err=%v", ok, err)
	}
	if err := s.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(ft.to) != 1 || ft.to[0] != "ops@example.com" {
		t.Fatalf("recipients = %v, want [ops@example.com]", ft.to)
	}
}

func TestDestinationSourceOnlyDeliversEnabledVerifiedOwned(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_ok":         destination("dst_ok", domain.DestinationEmail, "ok@example.com", true, now),
		"dst_off":        destination("dst_off", domain.DestinationEmail, "off@example.com", false, now),
		"dst_unverified": destination("dst_unverified", domain.DestinationEmail, "u@example.com", true, time.Time{}),
	}}
	src := NewDestinationSource(store, config.Gateway{}, "", "")

	for _, tc := range []struct {
		name  string
		id    domain.DestinationID
		found bool
	}{
		{"enabled and verified", "dst_ok", true},
		{"disabled", "dst_off", false},
		{"unverified", "dst_unverified", false},
		{"missing", "dst_missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := src.Sink(context.Background(), "ten_a", "alpha", domain.SinkID(tc.id))
			if err != nil || ok != tc.found {
				t.Fatalf("ok = %v (want %v) err = %v", ok, tc.found, err)
			}
		})
	}

	// A destination owned by another tenant is not resolved.
	if _, ok, _ := src.Sink(context.Background(), "ten_b", "alpha", "dst_ok"); ok {
		t.Fatal("resolved a destination under the wrong tenant")
	}
}

func TestDestinationSourceBuildsWebhook(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_w": destination("dst_w", domain.DestinationWebhook, ts.URL, true, now),
	}}
	// The test server is on loopback; opt in exactly as an operator would for a
	// trusted internal receiver. The hardened client is still used.
	cfg := config.Gateway{Security: config.Security{AllowPrivateNetworks: true}}
	src := NewDestinationSource(store, cfg, "", "")

	s, ok, err := src.Sink(context.Background(), "ten_a", "alpha", "dst_w")
	if err != nil || !ok {
		t.Fatalf("Sink ok=%v err=%v", ok, err)
	}
	if err := s.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if calls != 1 {
		t.Fatalf("webhook calls = %d, want 1", calls)
	}
}

// The operator allowlist is applied to stored webhook destinations at delivery
// time: an unlisted host is refused, a listed host is accepted.
func TestDestinationSourceEnforcesWebhookAllowlist(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_w": destination("dst_w", domain.DestinationWebhook, "https://blocked.example/x", true, now),
	}}
	cfg := config.Gateway{Security: config.Security{WebhookAllowHosts: []string{"allowed.example"}}}
	src := NewDestinationSource(store, cfg, "", "")

	if _, ok, err := src.Sink(context.Background(), "ten_a", "alpha", "dst_w"); err != nil || ok {
		t.Fatalf("unlisted host resolved: ok=%v err=%v", ok, err)
	}

	store.byID["dst_w"] = destination("dst_w", domain.DestinationWebhook, "https://allowed.example/x", true, now)
	if _, ok, err := src.Sink(context.Background(), "ten_a", "alpha", "dst_w"); err != nil || !ok {
		t.Fatalf("listed host refused: ok=%v err=%v", ok, err)
	}
}

func TestDestinationSourceStoreErrorSurfaces(t *testing.T) {
	boom := errors.New("boom")
	src := NewDestinationSource(&stubDestinations{err: boom}, config.Gateway{}, "", "")
	if _, _, err := src.Sink(context.Background(), "ten_a", "alpha", "dst"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

var _ app.DestinationStore = (*stubDestinations)(nil)

// stubSuppressions is a minimal app.SuppressionStore driven by a fixed set.
type stubSuppressions struct {
	byAddress map[string]bool
	err       error
}

func (s *stubSuppressions) Suppressed(_ context.Context, address string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.byAddress[address], nil
}

func (s *stubSuppressions) IngestProviderEvent(context.Context, app.ProviderEvent) (bool, error) {
	return true, nil
}

var _ app.SuppressionStore = (*stubSuppressions)(nil)

// TestDestinationSourceRefusesSuppressed proves the suppression list is enforced
// at delivery time: a suppressed address is refused permanently (dead-lettered,
// no retry) and the underlying transport is never called.
func TestDestinationSourceRefusesSuppressed(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_e": destination("dst_e", domain.DestinationEmail, "blocked@example.com", true, now),
	}}
	suppressed := &stubSuppressions{byAddress: map[string]bool{"blocked@example.com": true}}
	ft := &fakeTransport{}
	src := NewDestinationSource(store, config.Gateway{}, "", "").WithMailTransport(ft).WithSuppressions(suppressed)

	s, ok, err := src.Sink(context.Background(), "ten_a", "alpha", "dst_e")
	if err != nil || !ok {
		t.Fatalf("Sink ok=%v err=%v", ok, err)
	}
	err = s.Deliver(context.Background(), sampleEvent())
	if !app.IsSuppressed(err) || !app.IsPermanent(err) {
		t.Fatalf("err = %v, want a permanent suppression", err)
	}
	if len(ft.to) != 0 {
		t.Fatalf("suppressed address was sent to: %v", ft.to)
	}
}

// A suppression lookup failure is retryable: a transient store outage must not
// silently drop mail.
func TestDestinationSourceSuppressionStoreErrorIsRetryable(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_e": destination("dst_e", domain.DestinationEmail, "ok@example.com", true, now),
	}}
	suppressed := &stubSuppressions{byAddress: map[string]bool{}, err: errors.New("db down")}
	src := NewDestinationSource(store, config.Gateway{}, "", "").
		WithMailTransport(&fakeTransport{}).WithSuppressions(suppressed)

	s, _, _ := src.Sink(context.Background(), "ten_a", "alpha", "dst_e")
	if err := s.Deliver(context.Background(), sampleEvent()); err == nil || app.IsPermanent(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
}

// A verified address that is not suppressed is delivered normally.
func TestDestinationSourceAllowsNotSuppressed(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := &stubDestinations{byID: map[domain.DestinationID]domain.Destination{
		"dst_e": destination("dst_e", domain.DestinationEmail, "ok@example.com", true, now),
	}}
	src := NewDestinationSource(store, config.Gateway{}, "", "").
		WithMailTransport(&fakeTransport{}).
		WithSuppressions(&stubSuppressions{byAddress: map[string]bool{}})

	s, _, _ := src.Sink(context.Background(), "ten_a", "alpha", "dst_e")
	if err := s.Deliver(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}
