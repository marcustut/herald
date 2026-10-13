// Package memory provides in-memory adapters for local development and tests.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Credentials is an in-memory app.CredentialStore.
type Credentials struct {
	mu     sync.RWMutex
	byHash map[string]app.Credential
}

func NewCredentials() *Credentials {
	return &Credentials{byHash: map[string]app.Credential{}}
}

func (s *Credentials) Put(_ context.Context, c app.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash[c.Hash] = c
	return nil
}

func (s *Credentials) ByHash(_ context.Context, hash string) (app.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.byHash[hash]
	if !ok {
		return app.Credential{}, app.ErrCredentialNotFound
	}
	return c, nil
}

func (s *Credentials) ProjectCredential(_ context.Context, tenant domain.TenantID, project domain.ProjectID) (app.Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.byHash {
		if c.TenantID == tenant && c.Kind == domain.TokenProject && c.Allows(project) {
			return c, nil
		}
	}
	return app.Credential{}, app.ErrCredentialNotFound
}

var _ app.CredentialStore = (*Credentials)(nil)

type reportKey struct {
	tenant domain.TenantID
	key    domain.IdempotencyKey
}

// Reports is an in-memory app.ReportStore and app.DeliveryStore.
type Reports struct {
	mu         sync.RWMutex
	reports    map[domain.ReportID]domain.Report
	byKey      map[reportKey]domain.ReportID
	deliveries []domain.Delivery
}

func NewReports() *Reports {
	return &Reports{
		reports: map[domain.ReportID]domain.Report{},
		byKey:   map[reportKey]domain.ReportID{},
	}
}

func (s *Reports) Save(_ context.Context, rep domain.Report, ds []domain.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := reportKey{rep.TenantID, rep.IdempotencyKey}
	if _, ok := s.byKey[k]; ok {
		return app.ErrIdempotencyConflict
	}
	s.reports[rep.ID] = rep
	s.byKey[k] = rep.ID
	s.deliveries = append(s.deliveries, ds...)
	return nil
}

func (s *Reports) ByID(_ context.Context, id domain.ReportID) (domain.Report, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rep, ok := s.reports[id]
	return rep, ok, nil
}

func (s *Reports) ByIdempotencyKey(_ context.Context, tenant domain.TenantID, key domain.IdempotencyKey) (domain.Report, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byKey[reportKey{tenant, key}]
	if !ok {
		return domain.Report{}, false, nil
	}
	rep, ok := s.reports[id]
	return rep, ok, nil
}

func (s *Reports) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var claimed []domain.Delivery
	for i := range s.deliveries {
		if limit > 0 && len(claimed) >= limit {
			break
		}
		d := &s.deliveries[i]
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

func (s *Reports) Update(_ context.Context, del domain.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.deliveries {
		if s.deliveries[i].ID == del.ID {
			s.deliveries[i] = del
			return nil
		}
	}
	return app.ErrDeliveryNotFound
}

// Deliveries returns a copy of the stored delivery rows.
func (s *Reports) Deliveries() []domain.Delivery {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Delivery(nil), s.deliveries...)
}

// Report returns a stored report by id.
func (s *Reports) Report(id domain.ReportID) (domain.Report, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rep, ok := s.reports[id]
	return rep, ok
}

var (
	_ app.ReportStore   = (*Reports)(nil)
	_ app.DeliveryStore = (*Reports)(nil)
)
