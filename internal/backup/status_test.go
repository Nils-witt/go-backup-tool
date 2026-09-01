package backup

import (
	"errors"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func newTestStore() (*StatusStore, *config.Config) {
	cfg := &config.Config{
		Name:     "test",
		Interval: time.Minute,
		Targets: []config.Target{
			{ServerName: "sibling", Bucket: "b1", Kind: config.ServerKindRemote},
			{ServerName: "nas", Bucket: "b2", Kind: config.ServerKindLocal},
		},
	}

	return NewStatusStore([]*config.Config{cfg}), cfg
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

func TestOverallState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		states []RunState
		want   RunState
	}{
		{"all ok", []RunState{StateOK, StateOK}, StateOK},
		{"all failed", []RunState{StateFailed, StateFailed}, StateFailed},
		{"mixed", []RunState{StateOK, StateFailed}, StateIncomplete},
		{"single ok", []RunState{StateOK}, StateOK},
		{"single failed", []RunState{StateFailed}, StateFailed},
		// A target still running counts the same as failed here: only a
		// state of stateOK counts as succeeded, so overallState never
		// reports ok or incomplete for a job that isn't actually done yet.
		{"one running, one ok", []RunState{StateRunning, StateOK}, StateIncomplete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			targets := make([]TargetSnapshot, len(tt.states))
			for i, s := range tt.states {
				targets[i] = TargetSnapshot{State: s}
			}

			if got := overallState(targets); got != tt.want {
				t.Errorf("overallState(%v) = %q, want %q", tt.states, got, tt.want)
			}
		})
	}
}

func TestNewStatusStoreStartsIdle(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	snap := store.Snapshot()

	if len(snap) != 1 {
		t.Fatalf("snapshot() = %d jobs, want 1", len(snap))
	}

	j := snap[0]
	if j.Name != "test" || j.State != StateIdle || j.Interval != "1m0s" {
		t.Errorf("snapshot()[0] = %+v, want idle job %q with interval 1m0s", j, "test")
	}

	if len(j.Targets) != 2 {
		t.Fatalf("snapshot()[0].Targets = %d, want 2", len(j.Targets))
	}

	for i, tgt := range j.Targets {
		if tgt.State != StateIdle {
			t.Errorf("target[%d].State = %q, want idle", i, tgt.State)
		}
	}
}

func TestStatusStoreLifecycleSuccess(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	store.Starting("test")
	store.TargetDone("test", 0, nil)
	store.TargetDone("test", 1, nil)

	if got := store.Finished("test", nil, 2048); got != StateOK {
		t.Errorf("finished() = %q, want ok", got)
	}

	snap := store.Snapshot()[0]

	if snap.State != StateOK {
		t.Errorf("job State = %q, want ok", snap.State)
	}

	if snap.Duration == "" {
		t.Error("job Duration is empty after finished()")
	}

	if snap.Size != "2.0 KiB" {
		t.Errorf("job Size = %q, want %q", snap.Size, "2.0 KiB")
	}

	for i, tgt := range snap.Targets {
		if tgt.State != StateOK {
			t.Errorf("target[%d].State = %q, want ok", i, tgt.State)
		}
	}
}

// TestStatusStoreLifecycleIncomplete verifies that a run where some targets
// succeeded and others failed is reported as incomplete, not failed — the
// backup did land somewhere, so that's worth distinguishing from a total
// failure. Size is still recorded too, since staging must have produced a
// complete file for the successful target to have gotten a copy of it.
func TestStatusStoreLifecycleIncomplete(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	boom := errors.New("boom")

	store.Starting("test")
	store.TargetDone("test", 0, nil)
	store.TargetDone("test", 1, boom)

	if got := store.Finished("test", boom, 2048); got != StateIncomplete {
		t.Errorf("finished() = %q, want incomplete", got)
	}

	snap := store.Snapshot()[0]

	if snap.State != StateIncomplete || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {incomplete, boom}", snap.State, snap.Error)
	}

	if snap.Size != "2.0 KiB" {
		t.Errorf("job Size = %q, want %q (one target did get a complete copy)", snap.Size, "2.0 KiB")
	}

	if snap.Targets[0].State != StateOK {
		t.Errorf("target[0].State = %q, want ok", snap.Targets[0].State)
	}

	if snap.Targets[1].State != StateFailed || snap.Targets[1].Error != "boom" {
		t.Errorf("target[1] = {state: %q, error: %q}, want {failed, boom}", snap.Targets[1].State, snap.Targets[1].Error)
	}
}

// TestStatusStoreLifecycleTotalFailure verifies that a run where every
// target failed is still reported as failed (not incomplete), with no size
// reported since nothing succeeded.
func TestStatusStoreLifecycleTotalFailure(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	boom := errors.New("boom")

	store.Starting("test")
	store.TargetDone("test", 0, boom)
	store.TargetDone("test", 1, boom)

	if got := store.Finished("test", boom, 0); got != StateFailed {
		t.Errorf("finished() = %q, want failed", got)
	}

	snap := store.Snapshot()[0]

	if snap.State != StateFailed || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {failed, boom}", snap.State, snap.Error)
	}

	if snap.Size != "" {
		t.Errorf("job Size = %q after every target failed, want empty (no successful write to report)", snap.Size)
	}

	if snap.Targets[0].State != StateFailed || snap.Targets[1].State != StateFailed {
		t.Errorf("targets = %+v, want both failed", snap.Targets)
	}
}

func TestStatusStoreStartingResetsPriorError(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()
	boom := errors.New("boom")

	store.Starting("test")
	store.TargetDone("test", 0, boom)
	store.Finished("test", boom, 0)

	store.Starting("test")

	snap := store.Snapshot()[0]
	if snap.State != StateRunning || snap.Error != "" {
		t.Errorf("job after restart = {state: %q, error: %q}, want {running, \"\"}", snap.State, snap.Error)
	}

	if snap.Targets[0].State != StateRunning || snap.Targets[0].Error != "" {
		t.Errorf("target[0] after restart = {state: %q, error: %q}, want {running, \"\"}", snap.Targets[0].State, snap.Targets[0].Error)
	}
}

func TestStatusStoreUnknownJobAndTargetAreNoOps(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	// None of these name/index an existing job or target; they must not
	// panic and must leave the known job's status untouched.
	store.Starting("nope")
	store.TargetDone("nope", 0, nil)
	store.TargetDone("test", 99, nil)
	store.TargetDone("test", -1, nil)
	store.Finished("nope", nil, 0)

	snap := store.Snapshot()[0]
	if snap.State != StateIdle {
		t.Errorf("job State = %q after no-op calls, want idle", snap.State)
	}
}

func TestNewStatusStoreInitializesNextRunFromStartTime(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	cfg := &config.Config{Name: "test", Interval: time.Hour, StartTime: start}

	store := NewStatusStore([]*config.Config{cfg})

	if got := store.Snapshot()[0].NextRun; !got.Equal(start) {
		t.Errorf("snapshot()[0].NextRun = %v, want %v", got, start)
	}
}

func TestNewStatusStoreNextRunZeroWithoutStartTime(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	if got := store.Snapshot()[0].NextRun; !got.IsZero() {
		t.Errorf("snapshot()[0].NextRun = %v, want zero", got)
	}
}

func TestStatusStoreSetNextRun(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SetNextRun("test", want)

	if got := store.Snapshot()[0].NextRun; !got.Equal(want) {
		t.Errorf("snapshot()[0].NextRun = %v, want %v", got, want)
	}

	// Unknown job name must not panic and must not create an entry.
	store.SetNextRun("nope", want)
}

func TestStatusStoreSeedLastRunSuccess(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Second)

	store.SeedLastRun("test", start, end, StateOK, "", 2048)

	snap := store.Snapshot()[0]

	if snap.State != StateOK {
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

	store.SeedLastRun("test", start, end, StateFailed, "boom", 0)

	snap := store.Snapshot()[0]

	if snap.State != StateFailed || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {failed, boom}", snap.State, snap.Error)
	}

	if snap.Size != "" {
		t.Errorf("job Size = %q after seeding a failed run, want empty", snap.Size)
	}
}

func TestStatusStoreSeedLastRunIncomplete(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	start := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	end := start.Add(time.Second)

	store.SeedLastRun("test", start, end, StateIncomplete, "boom", 2048)

	snap := store.Snapshot()[0]

	if snap.State != StateIncomplete || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {incomplete, boom}", snap.State, snap.Error)
	}

	if snap.Size != "2.0 KiB" {
		t.Errorf("job Size = %q, want %q (an incomplete run still had a complete backup)", snap.Size, "2.0 KiB")
	}
}

func TestStatusStoreSeedLastRunUnknownJobIsNoOp(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	store.SeedLastRun("nope", time.Time{}, time.Time{}, StateOK, "", 0)

	snap := store.Snapshot()[0]
	if snap.State != StateIdle {
		t.Errorf("job State = %q after seeding an unknown job, want idle", snap.State)
	}
}

func TestStatusStoreSnapshotIsIndependentCopy(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore()

	snap := store.Snapshot()
	snap[0].Targets[0].State = StateFailed

	fresh := store.Snapshot()
	if fresh[0].Targets[0].State != StateIdle {
		t.Error("mutating a snapshot's target slice leaked back into the store")
	}
}
