package backup

import (
	"fmt"
	"sync"
	"time"
)

// runState is the lifecycle state of a job or target as shown in the web UI
// (see webui.go). A target's own state is always one of idle/running/ok/
// failed — stateIncomplete only ever applies at the job level, summarizing
// a run where its targets disagreed (see overallState).
type runState string

const (
	stateIdle       runState = "idle"       // configured, never run yet (or waiting for its next interval)
	stateRunning    runState = "running"    // currently executing
	stateOK         runState = "ok"         // last run succeeded (every target succeeded)
	stateIncomplete runState = "incomplete" // job only: last run succeeded on some targets but not all
	stateFailed     runState = "failed"     // last run failed (every target failed, or the run never reached the targets at all)
)

// targetSnapshot is one job target's current status, as reported over
// /api/status.
type targetSnapshot struct {
	Server string   `json:"server"`
	Bucket string   `json:"bucket"`
	Kind   string   `json:"kind"`
	State  runState `json:"state"`
	Error  string   `json:"error,omitempty"`
}

// jobSnapshot is one job's current status, as reported over /api/status.
type jobSnapshot struct {
	Name      string           `json:"name"`
	Interval  string           `json:"interval,omitempty"`
	State     runState         `json:"state"`
	LastStart time.Time        `json:"last_start"`
	LastEnd   time.Time        `json:"last_end"`
	NextRun   time.Time        `json:"next_run"` // zero if not (yet) scheduled, e.g. a one-shot job that already ran
	Duration  string           `json:"duration,omitempty"`
	Size      string           `json:"size,omitempty"` // encrypted object size from the last successful run; sticky across a later failure
	Error     string           `json:"error,omitempty"`
	Targets   []targetSnapshot `json:"targets"`
}

// statusStore tracks the live state of every configured job and its targets,
// updated by the runner as jobs execute and read by the web UI's HTTP
// handlers. Safe for concurrent use: jobs run concurrently with each other
// and with any number of status requests.
type statusStore struct {
	mu    sync.Mutex
	jobs  map[string]*jobSnapshot
	order []string // job names in config order, for stable UI listing
}

// newStatusStore builds a statusStore with one idle entry per job in jobs,
// preserving their config-file order.
func newStatusStore(jobs []*config) *statusStore {
	s := &statusStore{jobs: make(map[string]*jobSnapshot, len(jobs))}

	for _, j := range jobs {
		targets := make([]targetSnapshot, len(j.targets))
		for i, t := range j.targets {
			targets[i] = targetSnapshot{Server: t.serverName, Bucket: t.bucket, Kind: string(t.kind), State: stateIdle}
		}

		snap := &jobSnapshot{Name: j.name, Interval: intervalString(j.interval), State: stateIdle, Targets: targets}
		if !j.startTime.IsZero() {
			snap.NextRun = j.startTime
		}

		s.jobs[j.name] = snap
		s.order = append(s.order, j.name)
	}

	return s
}

// intervalString renders d for display, or "" for a job that doesn't repeat.
func intervalString(d time.Duration) string {
	if d <= 0 {
		return ""
	}

	return d.String()
}

// formatBytes renders n as a human-readable, binary (1024-based) size, e.g.
// "12.3 MB", matching the convention tools like du/ls -lh use.
func formatBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// starting marks name as running, resetting its (and its targets') last
// error so a fresh run starts clean.
func (s *statusStore) starting(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.State = stateRunning
	j.LastStart = time.Now()
	j.Error = ""

	for i := range j.Targets {
		j.Targets[i].State = stateRunning
		j.Targets[i].Error = ""
	}
}

// targetDone records the outcome of job name's target at index (index-aligned
// with the job's targets:, per runPipeline's targetErrs).
func (s *statusStore) targetDone(name string, index int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok || index < 0 || index >= len(j.Targets) {
		return
	}

	if err != nil {
		j.Targets[index].State = stateFailed
		j.Targets[index].Error = err.Error()

		return
	}

	j.Targets[index].State = stateOK
	j.Targets[index].Error = ""
}

// setNextRun records job name's next scheduled run time, for display in the
// web UI. Called by runner.schedule whenever it computes (or recomputes) the
// time it's about to wait for.
func (s *statusStore) setNextRun(name string, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.NextRun = next
}

// finished records job name's overall outcome and duration once its run
// completes, deriving the overall state from each target's own already-
// recorded outcome (see targetDone) rather than solely from err != nil: a
// job whose targets succeeded and failed in a mix is reported as
// incomplete, not failed — the backup did land somewhere. It returns the
// state it recorded (or "" if name is unknown), so a caller that persists
// its own summary of the run (see runner.recordLastRun) can stay
// consistent with the live store without re-deriving it.
//
// size is the encrypted object's byte count; it's recorded whenever at
// least one target succeeded (ok or incomplete), since staging must have
// produced one complete file of exactly that size for any target to have
// gotten a copy of it — a run that reached no target at all (every target
// failed, or the pipeline never got that far) leaves Size showing the last
// successful run's size instead of misrepresenting 0 or a partial count as
// a real file.
func (s *statusStore) finished(name string, err error, size int64) runState {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return ""
	}

	j.LastEnd = time.Now()
	j.Duration = j.LastEnd.Sub(j.LastStart).Round(time.Millisecond).String()
	j.State = overallState(j.Targets)

	j.Error = ""
	if err != nil {
		j.Error = err.Error()
	}

	if j.State == stateOK || j.State == stateIncomplete {
		j.Size = formatBytes(size)
	}

	return j.State
}

// overallState summarizes targets (a job's current per-target states) into
// one job-level runState: ok if every target succeeded, failed if none did,
// incomplete if some but not all did.
func overallState(targets []targetSnapshot) runState {
	succeeded := 0

	for _, t := range targets {
		if t.State == stateOK {
			succeeded++
		}
	}

	switch {
	case succeeded == len(targets):
		return stateOK
	case succeeded == 0:
		return stateFailed
	default:
		return stateIncomplete
	}
}

// seedLastRun initializes job name's snapshot from a previously persisted
// run (see readLastRun in schedule_state.go), so a restart's web UI can
// still show when the job last ran instead of reverting to "never" until it
// next runs. Called once at startup, before any job's own goroutine can
// call starting/finished, so it never races an actual run.
func (s *statusStore) seedLastRun(name string, run lastRun) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.State = run.State
	j.LastStart = run.Start
	j.LastEnd = run.End
	j.Duration = run.End.Sub(run.Start).Round(time.Millisecond).String()
	j.Error = run.Error

	if run.State == stateOK || run.State == stateIncomplete {
		j.Size = formatBytes(run.Size)
	}
}

// seedTargetRun initializes job name's target at index from a previously
// persisted result (see readTargetRuns in schedule_state.go), mirroring
// seedLastRun's reasoning one level down: a restart's web UI can still show
// each target's last outcome instead of every target reverting to "idle"
// until it next runs. Called once at startup, before any job's own goroutine
// can call starting/targetDone, so it never races an actual run.
func (s *statusStore) seedTargetRun(name string, index int, state runState, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok || index < 0 || index >= len(j.Targets) {
		return
	}

	j.Targets[index].State = state
	j.Targets[index].Error = errText
}

// snapshot returns every job's current status, in config-file order, each a
// copy safe to serialize (or otherwise use) without holding s.mu.
func (s *statusStore) snapshot() []jobSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]jobSnapshot, 0, len(s.order))

	for _, name := range s.order {
		j := *s.jobs[name]
		j.Targets = append([]targetSnapshot(nil), j.Targets...)
		out = append(out, j)
	}

	return out
}
