package app

import (
	"context"
	"fmt"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

var testNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

type fakeClock struct{ t time.Time }

func (c fakeClock) Now() time.Time { return c.t }

type seqSecrets struct{ n int }

func (s *seqSecrets) NewSecret() string {
	s.n++
	return fmt.Sprintf("secret-%016d", s.n)
}

type shortSecrets struct{}

func (shortSecrets) NewSecret() string { return "short" }

type fakeHasher struct{}

func (fakeHasher) Hash(raw string) string { return "h:" + raw }

type fakeStore struct {
	byHash map[string]Credential
	err    error
}

func newFakeStore() *fakeStore { return &fakeStore{byHash: map[string]Credential{}} }

func (s *fakeStore) Put(_ context.Context, c Credential) error {
	if s.err != nil {
		return s.err
	}
	s.byHash[c.Hash] = c
	return nil
}

func (s *fakeStore) ByHash(_ context.Context, hash string) (Credential, error) {
	if s.err != nil {
		return Credential{}, s.err
	}
	c, ok := s.byHash[hash]
	if !ok {
		return Credential{}, ErrCredentialNotFound
	}
	return c, nil
}

func (s *fakeStore) ProjectCredential(_ context.Context, tenant domain.TenantID, project domain.ProjectID) (Credential, error) {
	if s.err != nil {
		return Credential{}, s.err
	}
	for _, c := range s.byHash {
		if c.TenantID == tenant && c.Kind == domain.TokenProject && c.Allows(project) {
			return c, nil
		}
	}
	return Credential{}, ErrCredentialNotFound
}

func newIssuer(store *fakeStore) Issuer {
	return Issuer{Store: store, Hasher: fakeHasher{}, Secrets: &seqSecrets{}, Clock: fakeClock{testNow}}
}

type fakeSinks struct {
	sinks []domain.SinkID
	err   error
}

func (s *fakeSinks) EnabledSinks(context.Context, domain.TenantID, domain.ProjectID) ([]domain.SinkID, error) {
	return s.sinks, s.err
}

type fakeIDs struct{ n int }

func (g *fakeIDs) NewEventID() domain.EventID {
	g.n++
	return domain.EventID(fmt.Sprintf("evt_%d", g.n))
}

func (g *fakeIDs) NewDeliveryID() domain.DeliveryID {
	g.n++
	return domain.DeliveryID(fmt.Sprintf("del_%d", g.n))
}

func (g *fakeIDs) NewAccountID() domain.AccountID {
	g.n++
	return domain.AccountID(fmt.Sprintf("acct_%d", g.n))
}

func (g *fakeIDs) NewUserID() domain.UserID {
	g.n++
	return domain.UserID(fmt.Sprintf("usr_%d", g.n))
}

func (g *fakeIDs) NewTenantID() domain.TenantID {
	g.n++
	return domain.TenantID(fmt.Sprintf("ten_%d", g.n))
}

type eventKey struct {
	tenant  domain.TenantID
	project domain.ProjectID
	key     domain.IdempotencyKey
}

type fakeEvents struct {
	byKey      map[eventKey]domain.Event
	deliveries []domain.Delivery
	conflict   bool
	lookupErr  error
	saveErr    error
}

func newFakeEvents() *fakeEvents {
	return &fakeEvents{byKey: map[eventKey]domain.Event{}}
}

func (r *fakeEvents) Save(_ context.Context, rep domain.Event, ds []domain.Delivery) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	if r.conflict {
		return ErrIdempotencyConflict
	}
	k := eventKey{rep.TenantID, rep.ProjectID, rep.IdempotencyKey}
	if _, ok := r.byKey[k]; ok {
		return ErrIdempotencyConflict
	}
	r.byKey[k] = rep
	r.deliveries = append(r.deliveries, ds...)
	return nil
}

func (r *fakeEvents) ByIdempotencyKey(_ context.Context, tenant domain.TenantID, project domain.ProjectID, key domain.IdempotencyKey) (domain.Event, bool, error) {
	if r.lookupErr != nil {
		return domain.Event{}, false, r.lookupErr
	}
	rep, ok := r.byKey[eventKey{tenant, project, key}]
	return rep, ok, nil
}

func (r *fakeEvents) ByID(_ context.Context, id domain.EventID) (domain.Event, bool, error) {
	if r.lookupErr != nil {
		return domain.Event{}, false, r.lookupErr
	}
	for _, rep := range r.byKey {
		if rep.ID == id {
			return rep, true, nil
		}
	}
	return domain.Event{}, false, nil
}

type fakeDeliveries struct {
	rows      []domain.Delivery
	claimErr  error
	updateErr error
}

func newFakeDeliveries(ds ...domain.Delivery) *fakeDeliveries {
	return &fakeDeliveries{rows: ds}
}

func (s *fakeDeliveries) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error) {
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	s.reclaimExpired(now)
	var claimed []domain.Delivery
	for i := range s.rows {
		if limit > 0 && len(claimed) >= limit {
			break
		}
		d := &s.rows[i]
		if d.State != domain.DeliveryPending || d.NextAttemptAt.After(now) {
			continue
		}
		if err := d.Claim(now, lease); err != nil {
			continue
		}
		claimed = append(claimed, *d)
	}
	return claimed, nil
}

func (s *fakeDeliveries) Update(_ context.Context, del domain.Delivery) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	for i := range s.rows {
		if s.rows[i].ID == del.ID {
			s.rows[i] = del
			return nil
		}
	}
	return ErrDeliveryNotFound
}

func (s *fakeDeliveries) DeliveryByID(_ context.Context, id domain.DeliveryID) (domain.Delivery, bool, error) {
	for _, d := range s.rows {
		if d.ID == id {
			return d, true, nil
		}
	}
	return domain.Delivery{}, false, nil
}

func (s *fakeDeliveries) ListDeliveries(_ context.Context, f DeliveryFilter, limit int) ([]domain.Delivery, error) {
	var out []domain.Delivery
	for _, d := range s.rows {
		if f.ID != "" && d.ID != f.ID {
			continue
		}
		if f.State != "" && d.State != f.State {
			continue
		}
		if f.EventID != "" && d.EventID != f.EventID {
			continue
		}
		if f.SinkID != "" && d.SinkID != f.SinkID {
			continue
		}
		out = append(out, d)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeDeliveries) Requeue(_ context.Context, f DeliveryFilter, now time.Time) (int, error) {
	n := 0
	for i := range s.rows {
		d := &s.rows[i]
		if f.ID != "" && d.ID != f.ID {
			continue
		}
		if f.State != "" && d.State != f.State {
			continue
		}
		if f.EventID != "" && d.EventID != f.EventID {
			continue
		}
		if f.SinkID != "" && d.SinkID != f.SinkID {
			continue
		}
		if changed, err := d.Requeue(now); err != nil {
			return n, err
		} else if changed {
			n++
		}
	}
	return n, nil
}

func (s *fakeDeliveries) ReclaimExpired(_ context.Context, now time.Time) (int, error) {
	return s.reclaimExpired(now), nil
}

func (s *fakeDeliveries) reclaimExpired(now time.Time) int {
	n := 0
	for i := range s.rows {
		if s.rows[i].LeaseExpired(now) {
			_ = s.rows[i].Reclaim(now)
			n++
		}
	}
	return n
}

func (s *fakeDeliveries) row(id domain.DeliveryID) domain.Delivery {
	for _, d := range s.rows {
		if d.ID == id {
			return d
		}
	}
	return domain.Delivery{}
}

type fakeSink struct {
	id    domain.SinkID
	err   error
	calls int
	got   domain.Event
}

func (s *fakeSink) ID() domain.SinkID { return s.id }

func (s *fakeSink) Deliver(_ context.Context, r domain.Event) error {
	s.calls++
	s.got = r
	return s.err
}
