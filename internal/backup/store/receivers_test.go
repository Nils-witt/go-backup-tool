package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestReceiverConfigCRUD(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)

	r := ReceiverConfig{
		ID: "a", PublicKey: "pem", Path: "/srv/a", Retention: "30d", StaleAfter: "1d",
		StaleNotifications: []string{"ops"}, CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin",
	}

	if err := db.CreateReceiverConfig(ctx, r); err != nil {
		t.Fatalf("CreateReceiverConfig() error: %v", err)
	}

	if err := db.CreateReceiverConfig(ctx, r); !errors.Is(err, ErrReceiverExists) {
		t.Errorf("duplicate CreateReceiverConfig() error = %v, want ErrReceiverExists", err)
	}

	r.Path, r.Retention, r.StaleAfter, r.StaleNotifications = "/srv/b", "", "", nil
	r.DownloadNotifications = []string{"slack"}
	r.UpdatedBy, r.UpdatedAt = "frank", now.Add(time.Hour)
	r.CreatedBy = "ignored"

	if err := db.UpdateReceiverConfig(ctx, r); err != nil {
		t.Fatalf("UpdateReceiverConfig() error: %v", err)
	}

	got, ok, err := db.GetReceiverConfig(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("GetReceiverConfig() = %v, %v", ok, err)
	}

	if got.Path != "/srv/b" || got.Retention != "" || len(got.StaleNotifications) != 0 ||
		!slices.Equal(got.DownloadNotifications, []string{"slack"}) || got.CreatedBy != "erin" || got.UpdatedBy != "frank" {
		t.Errorf("after update = %+v", got)
	}
}

func TestReceiverConfigNotFoundAndDelete(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.UpdateReceiverConfig(ctx, ReceiverConfig{ID: "missing"}); !errors.Is(err, ErrReceiverNotFound) {
		t.Errorf("UpdateReceiverConfig(missing) error = %v, want ErrReceiverNotFound", err)
	}

	if err := db.CreateReceiverConfig(ctx, ReceiverConfig{ID: "a", PublicKey: "pem", Path: "/srv/a"}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteReceiverConfig(ctx, "a"); err != nil {
		t.Fatalf("DeleteReceiverConfig() error: %v", err)
	}

	if err := db.DeleteReceiverConfig(ctx, "a"); !errors.Is(err, ErrReceiverNotFound) {
		t.Errorf("second DeleteReceiverConfig() error = %v, want ErrReceiverNotFound", err)
	}

	list, err := db.ListReceiverConfigs(ctx)
	if err != nil || len(list) != 0 {
		t.Errorf("ListReceiverConfigs() = %+v, %v, want empty", list, err)
	}
}

func TestImportReceiverConfigsKeepsExistingRows(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.CreateReceiverConfig(ctx, ReceiverConfig{ID: "a", PublicKey: "pem", Path: "/edited", CreatedBy: "erin", UpdatedBy: "erin"}); err != nil {
		t.Fatal(err)
	}

	imported, err := db.ImportReceiverConfigs(ctx, []ReceiverConfig{
		{ID: "a", PublicKey: "pem", Path: "/from-yaml", CreatedBy: "config file", UpdatedBy: "config file"},
		{ID: "b", PublicKey: "pem", Path: "/b", CreatedBy: "config file", UpdatedBy: "config file"},
	})
	if err != nil {
		t.Fatalf("ImportReceiverConfigs() error: %v", err)
	}

	if !slices.Equal(imported, []string{"b"}) {
		t.Errorf("imported = %v, want [b]", imported)
	}

	list, err := db.ListReceiverConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 2 || list[0].ID != "a" || list[0].Path != "/edited" || list[1].ID != "b" {
		t.Errorf("receivers = %+v, want a (edited path kept) and b", list)
	}
}
