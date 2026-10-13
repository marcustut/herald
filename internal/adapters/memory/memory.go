// Package memory provides in-memory adapters for local development and tests.
package memory

import (
	"context"
	"sort"
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
	s.reclaimExpiredLocked(now)
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

func (s *Events) DeliveryByID(_ context.Context, id domain.DeliveryID) (domain.Delivery, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.deliveries {
		if d.ID == id {
			return d, true, nil
		}
	}
	return domain.Delivery{}, false, nil
}

func (s *Events) ListDeliveries(_ context.Context, f app.DeliveryFilter, limit int) ([]domain.Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.Delivery
	for _, d := range s.deliveries {
		if !matches(f, d) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Events) Requeue(_ context.Context, f app.DeliveryFilter, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := range s.deliveries {
		d := &s.deliveries[i]
		if !matches(f, *d) {
			continue
		}
		changed, err := d.Requeue(now)
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}

func (s *Events) ReclaimExpired(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reclaimExpiredLocked(now), nil
}

func (s *Events) Outstanding(_ context.Context, tenant domain.TenantID, project domain.ProjectID) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.deliveries {
		if d.State != domain.DeliveryPending && d.State != domain.DeliveryInflight {
			continue
		}
		e, ok := s.events[d.EventID]
		if ok && e.TenantID == tenant && e.ProjectID == project {
			n++
		}
	}
	return n, nil
}

func (s *Events) Backlog(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.deliveries {
		if d.State == domain.DeliveryPending || d.State == domain.DeliveryInflight {
			n++
		}
	}
	return n, nil
}

func (s *Events) reclaimExpiredLocked(now time.Time) int {
	n := 0
	for i := range s.deliveries {
		d := &s.deliveries[i]
		if d.LeaseExpired(now) {
			_ = d.Reclaim(now)
			n++
		}
	}
	return n
}

func matches(f app.DeliveryFilter, d domain.Delivery) bool {
	return (f.ID == "" || d.ID == f.ID) &&
		(f.State == "" || d.State == f.State) &&
		(f.EventID == "" || d.EventID == f.EventID) &&
		(f.SinkID == "" || d.SinkID == f.SinkID)
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
