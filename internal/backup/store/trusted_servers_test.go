package store

import (
	"errors"
	"testing"
	"time"
)

func TestTrustedServerCRUD(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)

	ts := TrustedServerConfig{ID: "b-id", Name: "nas", PublicKey: "pem", CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin"}

	if err := db.CreateTrustedServer(ctx, ts); err != nil {
		t.Fatalf("CreateTrustedServer() error: %v", err)
	}

	if err := db.CreateTrustedServer(ctx, ts); !errors.Is(err, ErrTrustedServerExists) {
		t.Errorf("duplicate CreateTrustedServer() error = %v, want ErrTrustedServerExists", err)
	}

	if err := db.CreateTrustedServer(ctx, TrustedServerConfig{ID: "a-id", Name: "office", PublicKey: "pem2"}); err != nil {
		t.Fatalf("CreateTrustedServer(a-id) error: %v", err)
	}

	ts.Name, ts.PublicKey, ts.UpdatedBy, ts.CreatedBy = "nas-2", "pem3", "frank", "ignored"

	if err := db.UpdateTrustedServer(ctx, ts); err != nil {
		t.Fatalf("UpdateTrustedServer() error: %v", err)
	}

	list, err := db.ListTrustedServers(ctx)
	if err != nil {
		t.Fatalf("ListTrustedServers() error: %v", err)
	}

	if len(list) != 2 || list[0].ID != "a-id" || list[1].ID != "b-id" {
		t.Fatalf("ListTrustedServers() = %+v, want a-id then b-id", list)
	}

	if got := list[1]; got.Name != "nas-2" || got.PublicKey != "pem3" || got.CreatedBy != "erin" || got.UpdatedBy != "frank" {
		t.Errorf("after update = %+v", got)
	}
}

func TestTrustedServerNotFoundAndDelete(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.UpdateTrustedServer(ctx, TrustedServerConfig{ID: "missing"}); !errors.Is(err, ErrTrustedServerNotFound) {
		t.Errorf("UpdateTrustedServer(missing) error = %v, want ErrTrustedServerNotFound", err)
	}

	if err := db.CreateTrustedServer(ctx, TrustedServerConfig{ID: "b-id", Name: "nas", PublicKey: "pem"}); err != nil {
		t.Fatalf("CreateTrustedServer() error: %v", err)
	}

	if err := db.DeleteTrustedServer(ctx, "b-id"); err != nil {
		t.Fatalf("DeleteTrustedServer() error: %v", err)
	}

	if err := db.DeleteTrustedServer(ctx, "b-id"); !errors.Is(err, ErrTrustedServerNotFound) {
		t.Errorf("second DeleteTrustedServer() error = %v, want ErrTrustedServerNotFound", err)
	}
}
