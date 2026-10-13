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

type eventKey struct {
	tenant  domain.TenantID
	project domain.ProjectID
	key     domain.IdempotencyKey
}

// Events is an in-memory app.EventStore and app.DeliveryStore.
type Events struct {
	mu         sync.RWMutex
	events     map[domain.EventID]domain.Event
	byKey      map[eventKey]domain.EventID
	deliveries []domain.Delivery
}

func NewEvents() *Events {
	return &Events{
		events: map[domain.EventID]domain.Event{},
		byKey:  map[eventKey]domain.EventID{},
	}
}

func (s *Events) Save(_ context.Context, rep domain.Event, ds []domain.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := eventKey{rep.TenantID, rep.ProjectID, rep.IdempotencyKey}
	if _, ok := s.byKey[k]; ok {
		return app.ErrIdempotencyConflict
	}
	s.events[rep.ID] = rep
	s.byKey[k] = rep.ID
	s.deliveries = append(s.deliveries, ds...)
	return nil
}

func (s *Events) ByID(_ context.Context, id domain.EventID) (domain.Event, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rep, ok := s.events[id]
	return rep, ok, nil
}

func (s *Events) ByIdempotencyKey(_ context.Context, tenant domain.TenantID, project domain.ProjectID, key domain.IdempotencyKey) (domain.Event, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byKey[eventKey{tenant, project, key}]
	if !ok {
		return domain.Event{}, false, nil
	}
	rep, ok := s.events[id]
	return rep, ok, nil
}

func (s *Events) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]domain.Delivery, error) {
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

func (s *Events) Update(_ context.Context, del domain.Delivery) error {
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
func (s *Events) Deliveries() []domain.Delivery {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Delivery(nil), s.deliveries...)
}

// Event returns a stored event by id.
func (s *Events) Event(id domain.EventID) (domain.Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rep, ok := s.events[id]
	return rep, ok
}

var (
	_ app.EventStore    = (*Events)(nil)
	_ app.DeliveryStore = (*Events)(nil)
)
