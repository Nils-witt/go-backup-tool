package settings

import (
	"errors"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

func TestStatusPushInterval(t *testing.T) {
	t.Parallel()

	if got := (StatusPush{}).Interval(); got != DefaultStatusPushIntervalSeconds*time.Second {
		t.Errorf("Interval() with no interval set = %v, want the default", got)
	}

	if got := (StatusPush{IntervalSeconds: 90}).Interval(); got != 90*time.Second {
		t.Errorf("Interval() = %v, want 1m30s", got)
	}
}

func TestManager_UpdateStatusPush(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	m := newTestManager(t, db)
	ctx := t.Context()

	m.UseServers(func(name string) error {
		if name != "central" {
			return errors.New("not a remote server")
		}

		return nil
	})

	if got := m.StatusPush(); got != (StatusPush{}) {
		t.Errorf("StatusPush() before any save = %+v, want disabled", got)
	}

	// A disabled push may keep naming a server that no longer works.
	if err := m.UpdateStatusPush(ctx, "erin", StatusPush{Server: "nas"}); err != nil {
		t.Errorf("UpdateStatusPush() disabled with an unusable server error: %v", err)
	}

	want := StatusPush{Enabled: true, Server: "central", IntervalSeconds: 60}
	if err := m.UpdateStatusPush(ctx, "erin", want); err != nil {
		t.Fatalf("UpdateStatusPush() error: %v", err)
	}

	if got := m.StatusPush(); got != want {
		t.Errorf("StatusPush() = %+v, want %+v", got, want)
	}

	select {
	case <-m.StatusPushChanged():
	default:
		t.Error("StatusPushChanged() didn't signal the update")
	}

	managed, err := m.GetStatusPush(ctx)
	if err != nil || managed.StatusPush != want || managed.UpdatedBy != "erin" || managed.UpdatedAt.IsZero() {
		t.Errorf("GetStatusPush() = (%+v, %v), want %+v updated by erin", managed, err, want)
	}

	// A fresh Manager over the same db loads what was saved.
	reloaded := newTestManager(t, db)
	reloaded.Load(ctx)

	if got := reloaded.StatusPush(); got != want {
		t.Errorf("StatusPush() after Load = %+v, want %+v", got, want)
	}
}

func TestManager_UpdateStatusPushRejectsInvalid(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	m.UseServers(func(name string) error {
		if name != "central" {
			return errors.New("not a remote server")
		}

		return nil
	})

	invalid := []struct {
		name string
		in   StatusPush
	}{
		{name: "enabled without a server", in: StatusPush{Enabled: true}},
		{name: "unusable server", in: StatusPush{Enabled: true, Server: "nas"}},
		{name: "interval too short", in: StatusPush{Server: "central", IntervalSeconds: MinStatusPushIntervalSeconds - 1}},
		{name: "interval too long", in: StatusPush{Server: "central", IntervalSeconds: MaxStatusPushIntervalSeconds + 1}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := m.UpdateStatusPush(t.Context(), "erin", tt.in); !errors.Is(err, ErrInvalid) {
				t.Errorf("UpdateStatusPush() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestManager_UpdateStatusPushWithoutStateDB(t *testing.T) {
	t.Parallel()

	m := NewManager(nil, nil, nil, notify.SMTPSettings{}, notify.GPGSettings{}, discardLogger)

	if err := m.UpdateStatusPush(t.Context(), "erin", StatusPush{}); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("UpdateStatusPush() without a state db error = %v, want ErrStoreUnavailable", err)
	}

	if _, err := m.GetStatusPush(t.Context()); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("GetStatusPush() without a state db error = %v, want ErrStoreUnavailable", err)
	}
}
