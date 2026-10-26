package store

import (
	"testing"
	"time"
)

func TestStatusPushSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if _, ok, err := db.GetStatusPushSettings(ctx); err != nil || ok {
		t.Fatalf("GetStatusPushSettings() on an empty db = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	want := StatusPushSettings{Enabled: true, Server: "central", IntervalSeconds: 45, UpdatedAt: time.Now(), UpdatedBy: "alice"}
	if err := db.SaveStatusPushSettings(ctx, want); err != nil {
		t.Fatalf("SaveStatusPushSettings() error: %v", err)
	}

	want.Server = "other"
	if err := db.SaveStatusPushSettings(ctx, want); err != nil {
		t.Fatalf("SaveStatusPushSettings() again error: %v", err)
	}

	got, ok, err := db.GetStatusPushSettings(ctx)
	if err != nil || !ok {
		t.Fatalf("GetStatusPushSettings() = (_, %v, %v), want (_, true, nil)", ok, err)
	}

	if got.Enabled != want.Enabled || got.Server != want.Server || got.IntervalSeconds != want.IntervalSeconds ||
		got.UpdatedAt.IsZero() || got.UpdatedBy != want.UpdatedBy {
		t.Errorf("GetStatusPushSettings() = %+v, want %+v", got, want)
	}
}
