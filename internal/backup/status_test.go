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
	store.finished("test", nil)

	snap := store.snapshot()[0]

	if snap.State != stateOK {
		t.Errorf("job State = %q, want ok", snap.State)
	}

	if snap.Duration == "" {
		t.Error("job Duration is empty after finished()")
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
	store.finished("test", boom)

	snap := store.snapshot()[0]

	if snap.State != stateFailed || snap.Error != "boom" {
		t.Errorf("job = {state: %q, error: %q}, want {failed, boom}", snap.State, snap.Error)
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
	store.finished("test", boom)

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
	store.finished("nope", nil)

	snap := store.snapshot()[0]
	if snap.State != stateIdle {
		t.Errorf("job State = %q after no-op calls, want idle", snap.State)
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
