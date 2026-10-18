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
		ID: "a", AllowedServers: []string{"s1"}, Path: "/srv/a", Retention: "30d", StaleAfter: "1d",
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

	if err := db.CreateReceiverConfig(ctx, ReceiverConfig{ID: "a", AllowedServers: []string{"s1"}, Path: "/srv/a"}); err != nil {
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

func TestListReceiversRelyingOnPublicKey(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Now()

	// Rows as receivers from before v8.0.0 stored them, with a public key.
	for _, m := range []receiverModel{
		{ID: "key-only", PublicKey: "pem", Path: "/srv/a", CreatedAt: now, UpdatedAt: now},
		{ID: "key-and-server", PublicKey: "pem", AllowedServers: []string{"s1"}, Path: "/srv/b", CreatedAt: now, UpdatedAt: now},
		{ID: "server-only", AllowedServers: []string{"s1"}, Path: "/srv/c", CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.db.WithContext(ctx).Create(&m).Error; err != nil {
			t.Fatalf("seeding %q: %v", m.ID, err)
		}
	}

	ids, err := db.ListReceiversRelyingOnPublicKey(ctx)
	if err != nil || !slices.Equal(ids, []string{"key-only"}) {
		t.Fatalf("ListReceiversRelyingOnPublicKey() = %v, %v, want [key-only]", ids, err)
	}

	// Allowing a server (an update) clears the key, which fixes the receiver.
	if err := db.UpdateReceiverConfig(ctx, ReceiverConfig{ID: "key-only", AllowedServers: []string{"s1"}, Path: "/srv/a", UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateReceiverConfig() error: %v", err)
	}

	if ids, err := db.ListReceiversRelyingOnPublicKey(ctx); err != nil || len(ids) != 0 {
		t.Errorf("after the update ListReceiversRelyingOnPublicKey() = %v, %v, want none", ids, err)
	}

	var key string
	if err := db.read.Raw("SELECT public_key FROM receivers WHERE id = ?", "key-only").Scan(&key).Error; err != nil || key != "" {
		t.Errorf("public_key after the update = %q, %v, want it cleared", key, err)
	}
}
