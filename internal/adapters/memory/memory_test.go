package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

func TestCredentials(t *testing.T) {
	ctx := context.Background()
	store := NewCredentials()
	if _, err := store.ByHash(ctx, "missing"); !errors.Is(err, app.ErrCredentialNotFound) {
		t.Fatalf("err = %v", err)
	}

	project := app.Credential{Hash: "h1", TenantID: "ten_1", Kind: domain.TokenProject, Projects: []domain.ProjectID{"axis"}}
	scoped := app.Credential{Hash: "h2", TenantID: "ten_1", Kind: domain.TokenScoped, Projects: []domain.ProjectID{"axis", "herald"}}
	for _, c := range []app.Credential{project, scoped} {
		if err := store.Put(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ByHash(ctx, "h1")
	if err != nil || got.Hash != "h1" {
		t.Fatalf("got %+v, err %v", got, err)
	}

	// ProjectCredential returns the project token, never the scoped one.
	pc, err := store.ProjectCredential(ctx, "ten_1", "axis")
	if err != nil || pc.Hash != "h1" {
		t.Fatalf("project credential = %+v, err %v", pc, err)
	}
	if _, err := store.ProjectCredential(ctx, "ten_1", "herald"); !errors.Is(err, app.ErrCredentialNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestReports(t *testing.T) {
	ctx := context.Background()
	store := NewReports()

	rep := domain.Report{ID: "rep_1", TenantID: "ten_1", IdempotencyKey: "run-1"}
	delivery := domain.Delivery{ID: "del_1", ReportID: "rep_1", SinkID: "email"}
	if err := store.Save(ctx, rep, []domain.Delivery{delivery}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.ByIdempotencyKey(ctx, "ten_1", "run-1")
	if err != nil || !ok || got.ID != "rep_1" {
		t.Fatalf("got %+v, ok %v, err %v", got, ok, err)
	}
	if _, ok, _ := store.ByIdempotencyKey(ctx, "ten_1", "other"); ok {
		t.Fatal("unknown key should not resolve")
	}

	if err := store.Save(ctx, rep, nil); !errors.Is(err, app.ErrIdempotencyConflict) {
		t.Fatalf("err = %v", err)
	}

	if _, ok := store.Report("rep_1"); !ok {
		t.Fatal("report not stored")
	}
	if got := store.Deliveries(); len(got) != 1 || got[0].ID != "del_1" {
		t.Fatalf("deliveries = %+v", got)
	}
}
