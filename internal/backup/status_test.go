package backup

import (
	"errors"
	"testing"
	"time"
)

func newTestStore() (*statusStore, *config) {
	cfg := &config{
		name:     "test",
		interval: time.Minute,
		targets: []target{
			{serverName: "primary", bucket: "b1", kind: serverKindS3},
			{serverName: "nas", bucket: "b2", kind: serverKindLocal},
		},
	}

	return newStatusStore([]*config{cfg}), cfg
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
	}

	for _, tt := range tests {
		if got := formatBytes(tt.n); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestNewStatusStoreStartsIdle(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	snap := store.snapshot()

	if len(snap) != 1 {
		t.Fatalf("snapshot() = %d jobs, want 1", len(snap))
	}

	j := snap[0]
	if j.Name != "test" || j.State != stateIdle || j.Interval != "1m0s" {
		t.Errorf("snapshot()[0] = %+v, want idle job %q with interval 1m0s", j, "test")
	}

	if len(j.Targets) != 2 {
		t.Fatalf("snapshot()[0].Targets = %d, want 2", len(j.Targets))
	}

	for i, tgt := range j.Targets {
		if tgt.State != stateIdle {
			t.Errorf("target[%d].State = %q, want idle", i, tgt.State)
		}
	}
}

func TestStatusStoreLifecycleSuccess(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	store.starting("test")
	store.targetDone("test", 0, nil)
	store.targetDone("test", 1, nil)
	store.finished("test", nil, 2048)

	snap := store.snapshot()[0]

	if snap.State != stateOK {
		t.Errorf("job State = %q, want ok", snap.State)
	}

	if snap.Duration == "" {
		t.Error("job Duration is empty after finished()")
	}

	if snap.Size != "2.0 KiB" {
		t.Errorf("job Size = %q, want %q", snap.Size, "2.0 KiB")
	}

	for i, tgt := range snap.Targets {
		if tgt.State != stateOK {
			t.Errorf("target[%d].State = %q, want ok", i, tgt.State)
		}
	}
}

func TestStatusStoreLifecycleFailure(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	boom := errors.New("boom")

	store.starting("test")
	store.targetDone("test", 0, nil)
	store.targetDone("test", 1, boom)
	store.finished("test", boom, 0)

	snap := store.snapshot()[0]

	if snap.State != stateFailed || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {failed, boom}", snap.State, snap.Error)
	}

	if snap.Size != "" {
		t.Errorf("job Size = %q after a failed run, want empty (no successful write to report)", snap.Size)
	}

	if snap.Targets[0].State != stateOK {
		t.Errorf("target[0].State = %q, want ok", snap.Targets[0].State)
	}

	if snap.Targets[1].State != stateFailed || snap.Targets[1].Error != "boom" {
		t.Errorf("target[1] = {state: %q, error: %q}, want {failed, boom}", snap.Targets[1].State, snap.Targets[1].Error)
	}
}

func TestStatusStoreStartingResetsPriorError(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	boom := errors.New("boom")

	store.starting("test")
	store.targetDone("test", 0, boom)
	store.finished("test", boom, 0)

	store.starting("test")

	snap := store.snapshot()[0]
	if snap.State != stateRunning || snap.Error != "" {
		t.Errorf("job after restart = {state: %q, error: %q}, want {running, \"\"}", snap.State, snap.Error)
	}

	if snap.Targets[0].State != stateRunning || snap.Targets[0].Error != "" {
		t.Errorf("target[0] after restart = {state: %q, error: %q}, want {running, \"\"}", snap.Targets[0].State, snap.Targets[0].Error)
	}
}

func TestStatusStoreUnknownJobAndTargetAreNoOps(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	// None of these name/index an existing job or target; they must not
	// panic and must leave the known job's status untouched.
	store.starting("nope")
	store.targetDone("nope", 0, nil)
	store.targetDone("test", 99, nil)
	store.targetDone("test", -1, nil)
	store.finished("nope", nil, 0)

	snap := store.snapshot()[0]
	if snap.State != stateIdle {
		t.Errorf("job State = %q after no-op calls, want idle", snap.State)
	}
}

func TestNewStatusStoreInitializesNextRunFromStartTime(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	cfg := &config{name: "test", interval: time.Hour, startTime: start}

	store := newStatusStore([]*config{cfg})

	if got := store.snapshot()[0].NextRun; !got.Equal(start) {
		t.Errorf("snapshot()[0].NextRun = %v, want %v", got, start)
	}
}

func TestNewStatusStoreNextRunZeroWithoutStartTime(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	if got := store.snapshot()[0].NextRun; !got.IsZero() {
		t.Errorf("snapshot()[0].NextRun = %v, want zero", got)
	}
}

func TestStatusStoreSetNextRun(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.setNextRun("test", want)

	if got := store.snapshot()[0].NextRun; !got.Equal(want) {
		t.Errorf("snapshot()[0].NextRun = %v, want %v", got, want)
	}

	// Unknown job name must not panic and must not create an entry.
	store.setNextRun("nope", want)
}

func TestStatusStoreSeedLastRunSuccess(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Second)

	store.seedLastRun("test", lastRun{Start: start, End: end, State: stateOK, Size: 2048})

	snap := store.snapshot()[0]

	if snap.State != stateOK {
		t.Errorf("job State = %q, want ok", snap.State)
	}

	if !snap.LastStart.Equal(start) || !snap.LastEnd.Equal(end) {
		t.Errorf("job LastStart/LastEnd = %v/%v, want %v/%v", snap.LastStart, snap.LastEnd, start, end)
	}

	if snap.Duration != "5s" {
		t.Errorf("job Duration = %q, want %q", snap.Duration, "5s")
	}

	if snap.Size != "2.0 KiB" {
		t.Errorf("job Size = %q, want %q", snap.Size, "2.0 KiB")
	}
}

func TestStatusStoreSeedLastRunFailure(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	end := start.Add(time.Second)

	store.seedLastRun("test", lastRun{Start: start, End: end, State: stateFailed, Error: "boom"})

	snap := store.snapshot()[0]

	if snap.State != stateFailed || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {failed, boom}", snap.State, snap.Error)
	}

	if snap.Size != "" {
		t.Errorf("job Size = %q after seeding a failed run, want empty", snap.Size)
	}
}

func TestStatusStoreSeedLastRunUnknownJobIsNoOp(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	store.seedLastRun("nope", lastRun{State: stateOK})

	snap := store.snapshot()[0]
	if snap.State != stateIdle {
		t.Errorf("job State = %q after seeding an unknown job, want idle", snap.State)
	}
}

func TestStatusStoreSnapshotIsIndependentCopy(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	snap := store.snapshot()
	snap[0].Targets[0].State = stateFailed

	fresh := store.snapshot()
	if fresh[0].Targets[0].State != stateIdle {
		t.Error("mutating a snapshot's target slice leaked back into the store")
	}
}
