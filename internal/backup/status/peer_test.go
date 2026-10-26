package status

import (
	"testing"
	"time"
)

// trustAll names every instance after its id.
func trustAll(id string) (string, bool) { return "srv-" + id, true }

func TestPeerStoreUpsert(t *testing.T) {
	t.Parallel()

	s := NewPeerStore()
	changed := s.Changed()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	s.Upsert(PeerReport{
		ID: "b", InstanceName: "backup-b", Interval: 30 * time.Second, ReceivedAt: at,
		Jobs:      []JobSnapshot{{Name: "db", State: StateOK, Targets: []TargetSnapshot{{Server: "nas", State: StateOK}}}},
		Receivers: []ReceiverSnapshot{{ID: "r", State: StateIdle}},
	})
	s.Upsert(PeerReport{ID: "a", InstanceName: "backup-a", Interval: time.Minute, ReceivedAt: at})

	select {
	case <-changed:
	default:
		t.Error("Changed() didn't fire on Upsert")
	}

	snap := s.Snapshot(at.Add(time.Second), trustAll)
	if len(snap) != 2 || snap[0].ID != "a" || snap[1].ID != "b" {
		t.Fatalf("Snapshot() = %+v, want a then b (sorted by name)", snap)
	}

	b := snap[1]
	if b.Name != "srv-b" || b.InstanceName != "backup-b" || b.Interval != "30s" || !b.LastSeen.Equal(at) || b.Offline {
		t.Errorf("Snapshot()[1] = %+v, want srv-b/backup-b, 30s, last seen %v, online", b, at)
	}

	if len(b.Jobs) != 1 || len(b.Jobs[0].Targets) != 1 || len(b.Receivers) != 1 {
		t.Errorf("Snapshot()[1] jobs/receivers = %+v / %+v, want one each", b.Jobs, b.Receivers)
	}
}

func TestPeerStoreSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	s := NewPeerStore()
	at := time.Now()
	s.Upsert(PeerReport{
		ID: "a", ReceivedAt: at,
		Jobs: []JobSnapshot{{Name: "db", State: StateOK, Targets: []TargetSnapshot{{Server: "nas", State: StateOK}}}},
	})

	s.Snapshot(at, trustAll)[0].Jobs[0].Targets[0].State = StateFailed

	if got := s.Snapshot(at, trustAll)[0].Jobs[0].Targets[0].State; got != StateOK {
		t.Errorf("target state after modifying a snapshot = %q, want ok", got)
	}
}

func TestPeerStoreSnapshotOffline(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		interval time.Duration
		silence  time.Duration
		expected bool
	}{
		{name: "within three intervals", interval: 10 * time.Minute, silence: 29 * time.Minute, expected: false},
		{name: "past three intervals", interval: 10 * time.Minute, silence: 31 * time.Minute, expected: true},
		{name: "short interval within the minimum", interval: 10 * time.Second, silence: 90 * time.Second, expected: false},
		{name: "short interval past the minimum", interval: 10 * time.Second, silence: 3 * time.Minute, expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewPeerStore()
			s.Upsert(PeerReport{ID: "a", Interval: tt.interval, ReceivedAt: at})

			if got := s.Snapshot(at.Add(tt.silence), trustAll)[0].Offline; got != tt.expected {
				t.Errorf("Offline after %v at a %v interval = %v, want %v", tt.silence, tt.interval, got, tt.expected)
			}
		})
	}
}

func TestPeerStoreSnapshotSkipsUntrusted(t *testing.T) {
	t.Parallel()

	s := NewPeerStore()
	s.Upsert(PeerReport{ID: "a", ReceivedAt: time.Now()})
	s.Upsert(PeerReport{ID: "gone", ReceivedAt: time.Now()})

	snap := s.Snapshot(time.Now(), func(id string) (string, bool) { return id, id != "gone" })
	if len(snap) != 1 || snap[0].ID != "a" {
		t.Errorf("Snapshot() = %+v, want only a", snap)
	}
}

func TestPeerStoreRemove(t *testing.T) {
	t.Parallel()

	s := NewPeerStore()
	s.Upsert(PeerReport{ID: "a", ReceivedAt: time.Now()})

	changed := s.Changed()

	if !s.Remove("a") {
		t.Error("Remove(a) = false, want true")
	}

	select {
	case <-changed:
	default:
		t.Error("Changed() didn't fire on Remove")
	}

	if s.Remove("a") {
		t.Error("Remove(a) a second time = true, want false")
	}

	if snap := s.Snapshot(time.Now(), trustAll); len(snap) != 0 {
		t.Errorf("Snapshot() after Remove = %+v, want empty", snap)
	}
}

func TestPeerStoreNil(t *testing.T) {
	t.Parallel()

	var s *PeerStore

	if snap := s.Snapshot(time.Now(), trustAll); snap == nil || len(snap) != 0 {
		t.Errorf("nil store Snapshot() = %#v, want an empty, non-nil slice", snap)
	}
}
