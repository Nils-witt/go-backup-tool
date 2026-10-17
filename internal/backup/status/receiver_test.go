package status

import (
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func TestReceiverStatusStoreSeedLastEventSuccess(t *testing.T) {
	t.Parallel()

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := NewReceiverStore(receivers)

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SeedLastEvent("a", "obj.gpg", at, true, "")

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %+v, want 1 entry", snap)
	}

	if snap[0].State != StateOK || snap[0].LastKey != "obj.gpg" || !snap[0].LastSeen.Equal(at) || snap[0].Error != "" {
		t.Errorf("snapshot()[0] = %+v, want state ok, last_key obj.gpg, last_seen %v, no error", snap[0], at)
	}
}

func TestReceiverStatusStoreSeedLastEventFailure(t *testing.T) {
	t.Parallel()

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := NewReceiverStore(receivers)

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SeedLastEvent("a", "obj.gpg", at, false, "disk full")

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %+v, want 1 entry", snap)
	}

	if snap[0].State != StateFailed || snap[0].Error != "disk full" {
		t.Errorf("snapshot()[0] = %+v, want state failed with error %q", snap[0], "disk full")
	}
}

func TestReceiverStatusStoreSeedLastEventUnknownReceiverIsNoop(t *testing.T) {
	t.Parallel()

	store := NewReceiverStore(map[string]config.ResolvedReceiver{})

	store.SeedLastEvent("does-not-exist", "", time.Time{}, true, "")

	if snap := store.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot() = %+v, want none", snap)
	}
}

func TestReceiverStatusStoreChangedFiresOnRecord(t *testing.T) {
	t.Parallel()

	s := NewReceiverStore(map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}})

	ch := s.Changed()
	s.Record("a", "backup.gpg", nil)
	assertClosed(t, ch, "Record")

	ch = s.Changed()
	s.SeedLastEvent("a", "backup.gpg", time.Now(), true, "")
	assertClosed(t, ch, "SeedLastEvent")
}
