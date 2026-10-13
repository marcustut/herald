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

func (g *fakeIDs) NewReportID() domain.ReportID {
	g.n++
	return domain.ReportID(fmt.Sprintf("rep_%d", g.n))
}

func (g *fakeIDs) NewDeliveryID() domain.DeliveryID {
	g.n++
	return domain.DeliveryID(fmt.Sprintf("del_%d", g.n))
}

type reportKey struct {
	tenant domain.TenantID
	key    domain.IdempotencyKey
}

type fakeReports struct {
	byKey      map[reportKey]domain.Report
	deliveries []domain.Delivery
	conflict   bool
	lookupErr  error
	saveErr    error
}

func newFakeReports() *fakeReports {
	return &fakeReports{byKey: map[reportKey]domain.Report{}}
}

func (r *fakeReports) Save(_ context.Context, rep domain.Report, ds []domain.Delivery) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	if r.conflict {
		return ErrIdempotencyConflict
	}
	k := reportKey{rep.TenantID, rep.IdempotencyKey}
	if _, ok := r.byKey[k]; ok {
		return ErrIdempotencyConflict
	}
	r.byKey[k] = rep
	r.deliveries = append(r.deliveries, ds...)
	return nil
}

func (r *fakeReports) ByIdempotencyKey(_ context.Context, tenant domain.TenantID, key domain.IdempotencyKey) (domain.Report, bool, error) {
	if r.lookupErr != nil {
		return domain.Report{}, false, r.lookupErr
	}
	rep, ok := r.byKey[reportKey{tenant, key}]
	return rep, ok, nil
}

func (r *fakeReports) ByID(_ context.Context, id domain.ReportID) (domain.Report, bool, error) {
	if r.lookupErr != nil {
		return domain.Report{}, false, r.lookupErr
	}
	for _, rep := range r.byKey {
		if rep.ID == id {
			return rep, true, nil
		}
	}
	return domain.Report{}, false, nil
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
	got   domain.Report
}

func (s *fakeSink) ID() domain.SinkID { return s.id }

func (s *fakeSink) Deliver(_ context.Context, r domain.Report) error {
	s.calls++
	s.got = r
	return s.err
}
