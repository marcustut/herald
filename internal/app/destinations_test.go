package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

type fakeDestinationStore struct {
	byID map[domain.DestinationID]domain.Destination
}

func newFakeDestinationStore() *fakeDestinationStore {
	return &fakeDestinationStore{byID: map[domain.DestinationID]domain.Destination{}}
}

func (s *fakeDestinationStore) PutDestination(_ context.Context, d domain.Destination) error {
	if existing, ok := s.byID[d.ID]; ok && !existing.OwnedBy(d.TenantID, d.ProjectID) {
		return ErrDestinationNotFound
	}
	s.byID[d.ID] = d
	return nil
}

func (s *fakeDestinationStore) Destination(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) (domain.Destination, bool, error) {
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return domain.Destination{}, false, nil
	}
	return d, true, nil
}

func (s *fakeDestinationStore) DestinationsByProject(_ context.Context, tenant domain.TenantID, project domain.ProjectID) ([]domain.Destination, error) {
	var out []domain.Destination
	for _, d := range s.byID {
		if d.OwnedBy(tenant, project) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *fakeDestinationStore) DeleteDestination(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID) error {
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return ErrDestinationNotFound
	}
	delete(s.byID, id)
	return nil
}

func (s *fakeDestinationStore) MarkVerified(_ context.Context, tenant domain.TenantID, project domain.ProjectID, id domain.DestinationID, now time.Time) error {
	d, ok := s.byID[id]
	if !ok || !d.OwnedBy(tenant, project) {
		return ErrDestinationNotFound
	}
	d.VerifiedAt = now
	s.byID[id] = d
	return nil
}

type fakeDestVerifications struct {
	byHash map[string]DestinationVerificationRecord
}

func newFakeDestVerifications() *fakeDestVerifications {
	return &fakeDestVerifications{byHash: map[string]DestinationVerificationRecord{}}
}

func (s *fakeDestVerifications) PutDestinationVerification(_ context.Context, r DestinationVerificationRecord) error {
	s.byHash[r.Hash] = r
	return nil
}

func (s *fakeDestVerifications) ConsumeDestinationVerification(_ context.Context, hash string, now time.Time) (DestinationVerificationRecord, error) {
	r, ok := s.byHash[hash]
	if !ok || !r.UsedAt.IsZero() || !r.ExpiresAt.After(now) {
		return DestinationVerificationRecord{}, ErrDestinationVerificationNotFound
	}
	r.UsedAt = now
	s.byHash[hash] = r
	return r, nil
}

type destinationsFixture struct {
	uc     Destinations
	clock  *stepClock
	store  *fakeDestinationStore
	tokens *fakeDestVerifications
	mail   *fakeMailer
}

func newDestinationsFixture(t *testing.T, projects ...domain.Project) *destinationsFixture {
	t.Helper()
	mail := &fakeMailer{}
	store := newFakeDestinationStore()
	tokens := newFakeDestVerifications()
	projectsStore := newFakeProjectStore()
	for _, p := range projects {
		if err := projectsStore.PutProject(context.Background(), p); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}
	clock := &stepClock{t: testNow}
	return &destinationsFixture{
		uc: Destinations{
			Projects: projectsStore,
			Store:    store,
			Tokens:   tokens,
			Mailer:   mail,
			Secrets:  &seqSecrets{},
			Hasher:   fakeHasher{},
			IDs:      &fakeIDs{},
			Clock:    clock,
			LinkBase: "https://cloud.example.com",
		},
		clock:  clock,
		store:  store,
		tokens: tokens,
		mail:   mail,
	}
}

func testProject(id domain.ProjectID, tenant domain.TenantID) domain.Project {
	return domain.Project{ID: id, TenantID: tenant, Name: string(id), CreatedAt: testNow}
}

func TestCreateEmailDestinationStartsUnverified(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	dst, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationEmail, "Ops@Example.COM")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if dst.Verified() || !dst.Enabled {
		t.Fatalf("new email destination = %+v, want enabled and unverified", dst)
	}
	if dst.Target != "Ops@example.com" {
		t.Fatalf("target = %q, want normalized address", dst.Target)
	}
	if len(f.mail.msgs) != 1 || f.mail.msgs[0].To != "Ops@example.com" {
		t.Fatalf("mail = %+v, want one verification message", f.mail.msgs)
	}
	raw := f.mail.token(t)
	var rec DestinationVerificationRecord
	for _, r := range f.tokens.byHash {
		rec = r
	}
	if rec.Hash != f.uc.Hasher.Hash(raw) || rec.Hash == raw {
		t.Fatalf("stored verification token hash = %q raw = %q", rec.Hash, raw)
	}
	if rec.DestinationID != dst.ID || rec.TenantID != "ten_a" || rec.ProjectID != "alpha" {
		t.Fatalf("verification record owner = %+v, want the destination's owner", rec)
	}
	if !strings.Contains(f.mail.msgs[0].Text, "/v1/destinations/verify?token=") {
		t.Fatalf("mail text = %q", f.mail.msgs[0].Text)
	}
}

func TestCreateWebhookVerifiedImmediately(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	dst, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationWebhook, "https://hooks.example.com/x")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !dst.Verified() {
		t.Fatal("webhook should be verified at creation")
	}
	if len(f.mail.msgs) != 0 {
		t.Fatalf("webhook creation mailed %+v, want none", f.mail.msgs)
	}
}

func TestCreateDestinationRejectsForeignProject(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_b"))
	if _, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationEmail, "ops@example.com"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
	if len(f.mail.msgs) != 0 {
		t.Fatal("a foreign project must not trigger mail")
	}
}

func TestCreateDestinationRejectsBadTarget(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	if _, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationEmail, "not-an-address"); !errors.Is(err, domain.ErrInvalidDestination) {
		t.Fatalf("err = %v, want ErrInvalidDestination", err)
	}
	if _, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationWebhook, "file:///etc/passwd"); !errors.Is(err, domain.ErrInvalidDestination) {
		t.Fatalf("webhook err = %v, want ErrInvalidDestination", err)
	}
}

func TestConfirmVerificationMarksVerifiedOnce(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	dst, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationEmail, "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	raw := f.mail.token(t)

	if err := f.uc.ConfirmVerification(context.Background(), raw); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	stored := f.store.byID[dst.ID]
	if !stored.VerifiedAt.Equal(testNow) {
		t.Fatalf("verified_at = %v, want %v", stored.VerifiedAt, testNow)
	}
	if err := f.uc.ConfirmVerification(context.Background(), raw); !errors.Is(err, ErrDestinationVerificationNotFound) {
		t.Fatalf("replay err = %v, want ErrDestinationVerificationNotFound", err)
	}
	if err := f.uc.ConfirmVerification(context.Background(), "short"); !errors.Is(err, ErrDestinationVerificationNotFound) {
		t.Fatalf("malformed err = %v, want ErrDestinationVerificationNotFound", err)
	}
}

func TestConfirmVerificationExpires(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	dst, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationEmail, "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	raw := f.mail.token(t)
	f.clock.advance(DefaultVerificationTTL + time.Second)

	if err := f.uc.ConfirmVerification(context.Background(), raw); !errors.Is(err, ErrDestinationVerificationNotFound) {
		t.Fatalf("expired err = %v, want ErrDestinationVerificationNotFound", err)
	}
	if f.store.byID[dst.ID].Verified() {
		t.Fatal("expired verification must not mark the destination verified")
	}
}

func TestRequestVerificationResendAndRejectWebhook(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	hook, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationWebhook, "https://hooks.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.uc.RequestVerification(context.Background(), "ten_a", "alpha", hook.ID); !errors.Is(err, ErrNotVerifiable) {
		t.Fatalf("webhook verify err = %v, want ErrNotVerifiable", err)
	}

	email, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationEmail, "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.uc.RequestVerification(context.Background(), "ten_a", "alpha", email.ID); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if len(f.mail.msgs) != 2 {
		t.Fatalf("mail count = %d, want 2", len(f.mail.msgs))
	}
	// The freshest link still verifies.
	if err := f.uc.ConfirmVerification(context.Background(), f.mail.token(t)); err != nil {
		t.Fatalf("confirm resent: %v", err)
	}
}

func TestDeleteAndListAreScoped(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"), testProject("beta", "ten_b"))
	dst, err := f.uc.Create(context.Background(), "ten_a", "alpha", domain.DestinationWebhook, "https://hooks.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if list, err := f.uc.List(context.Background(), "ten_a", "alpha"); err != nil || len(list) != 1 {
		t.Fatalf("list = %v err = %v", list, err)
	}
	if _, err := f.uc.List(context.Background(), "ten_b", "alpha"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("foreign list err = %v, want ErrProjectNotFound", err)
	}
	if err := f.uc.Delete(context.Background(), "ten_b", "beta", dst.ID); !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("foreign delete err = %v, want ErrDestinationNotFound", err)
	}
	if _, ok, _ := f.store.Destination(context.Background(), "ten_a", "alpha", dst.ID); !ok {
		t.Fatal("foreign delete removed the owner's destination")
	}
	if err := f.uc.Delete(context.Background(), "ten_a", "alpha", dst.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestConfirmUnknownDestination(t *testing.T) {
	f := newDestinationsFixture(t, testProject("alpha", "ten_a"))
	if err := f.uc.ConfirmVerification(context.Background(), strings.Repeat("a", 40)); !errors.Is(err, ErrDestinationVerificationNotFound) {
		t.Fatalf("err = %v, want ErrDestinationVerificationNotFound", err)
	}
}
