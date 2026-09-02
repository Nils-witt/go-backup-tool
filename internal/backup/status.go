package backup

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// RunState is the lifecycle state of a job or target as shown in the web UI
// (see webui.go). A target's own state is always one of idle/running/ok/
// failed — stateIncomplete only ever applies at the job level, summarizing
// a run where its targets disagreed (see overallState).
type RunState string

// The RunState values a job or target can be in.
const (
	StateIdle       RunState = "idle"       // configured, never run yet (or waiting for its next interval)
	StateRunning    RunState = "running"    // currently executing
	StateOK         RunState = "ok"         // last run succeeded (every target succeeded)
	StateIncomplete RunState = "incomplete" // job only: last run succeeded on some targets but not all
	StateFailed     RunState = "failed"     // last run failed (every target failed, or the run never reached the targets at all)
)

// TargetSnapshot is one job target's current status, as reported over
// /api/status.
type TargetSnapshot struct {
	Server string   `json:"server"`
	Bucket string   `json:"bucket"`
	Kind   string   `json:"kind"`
	State  RunState `json:"state"`
	Error  string   `json:"error,omitempty"`
}

// JobSnapshot is one job's current status, as reported over /api/status.
type JobSnapshot struct {
	Name      string           `json:"name"`
	Interval  string           `json:"interval,omitempty"`
	State     RunState         `json:"state"`
	LastStart time.Time        `json:"last_start"`
	LastEnd   time.Time        `json:"last_end"`
	NextRun   time.Time        `json:"next_run"` // zero if not (yet) scheduled, e.g. a one-shot job that already ran
	Duration  string           `json:"duration,omitempty"`
	Size      string           `json:"size,omitempty"` // encrypted object size from the last successful run; sticky across a later failure
	Error     string           `json:"error,omitempty"`
	Targets   []TargetSnapshot `json:"targets"`
}

// StatusStore tracks the live state of every configured job and its targets,
// updated as jobs execute (see the pipeline package) and read by the web UI's
// HTTP handlers. Safe for concurrent use: jobs run concurrently with each
// other and with any number of status requests.
type StatusStore struct {
	mu    sync.Mutex
	jobs  map[string]*JobSnapshot
	order []string // job names in config order, for stable UI listing
}

// NewStatusStore builds a StatusStore with one idle entry per job in jobs,
// preserving their config-file order.
func NewStatusStore(jobs []*config.Config) *StatusStore {
	s := &StatusStore{jobs: make(map[string]*JobSnapshot, len(jobs))}

	for _, j := range jobs {
		targets := make([]TargetSnapshot, len(j.Targets))
		for i, t := range j.Targets {
			targets[i] = TargetSnapshot{Server: t.ServerName, Bucket: t.Bucket, Kind: string(t.Kind), State: StateIdle}
		}

		snap := &JobSnapshot{Name: j.Name, Interval: intervalString(j.Interval), State: StateIdle, Targets: targets}
		if !j.StartTime.IsZero() {
			snap.NextRun = j.StartTime
		}

		s.jobs[j.Name] = snap
		s.order = append(s.order, j.Name)
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
	return FormatSize(n, 1024, "KMGTPE", true)
}

// FormatSize renders n as a human-readable size, dividing repeatedly by unit
// (1024 for binary, 1000 for decimal) and picking the matching letter from
// suffixes; binarySuffix appends "iB" (e.g. "MiB") instead of "B" (e.g.
// "MB"). Shared by formatBytes here and pipeline's formatReportBytes, which
// otherwise duplicate this exact algorithm for their own base/suffix
// conventions.
func FormatSize(n, unit int64, suffixes string, binarySuffix bool) string {
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := unit, 0

	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}

	if binarySuffix {
		return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), suffixes[exp])
	}

	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), suffixes[exp])
}

// SetOutcome sets *state/*errText to reflect err: failed/err.Error() when
// err is non-nil, ok/"" otherwise. Shared by StatusStore.TargetDone (job
// targets) and ReceiverStatusStore.Record (receivers), which track the same
// shape of live status for two different kinds of item, and by
// pipeline.Runner's target-run persistence.
func SetOutcome(state *RunState, errText *string, err error) {
	if err != nil {
		*state = StateFailed
		*errText = err.Error()

		return
	}

	*state = StateOK
	*errText = ""
}

// setSizeIfSucceeded sets *dst to size's human-readable form when state
// indicates at least one target succeeded (ok or incomplete), leaving *dst
// unchanged otherwise (a job's displayed size is sticky across a later
// failed run — see Finished's doc comment). Shared by Finished and
// SeedLastRun.
func setSizeIfSucceeded(dst *string, state RunState, size int64) {
	if state == StateOK || state == StateIncomplete {
		*dst = formatBytes(size)
	}
}

// Starting marks name as running, resetting its (and its targets') last
// error so a fresh run starts clean.
func (s *StatusStore) Starting(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.State = StateRunning
	j.LastStart = time.Now()
	j.Error = ""

	for i := range j.Targets {
		j.Targets[i].State = StateRunning
		j.Targets[i].Error = ""
	}
}

// TargetDone records the outcome of job name's target at index (index-aligned
// with the job's targets:, per runPipeline's targetErrs).
func (s *StatusStore) TargetDone(name string, index int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok || index < 0 || index >= len(j.Targets) {
		return
	}

	SetOutcome(&j.Targets[index].State, &j.Targets[index].Error, err)
}

// SetNextRun records job name's next scheduled run time, for display in the
// web UI. Called by runner.schedule whenever it computes (or recomputes) the
// time it's about to wait for.
func (s *StatusStore) SetNextRun(name string, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.NextRun = next
}

// Finished records job name's overall outcome and duration once its run
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
func (s *StatusStore) Finished(name string, err error, size int64) RunState {
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

	setSizeIfSucceeded(&j.Size, j.State, size)

	return j.State
}

// overallState summarizes targets (a job's current per-target states) into
// one job-level runState: ok if every target succeeded, failed if none did,
// incomplete if some but not all did.
func overallState(targets []TargetSnapshot) RunState {
	succeeded := 0

	for _, t := range targets {
		if t.State == StateOK {
			succeeded++
		}
	}

	switch {
	case succeeded == len(targets):
		return StateOK
	case succeeded == 0:
		return StateFailed
	default:
		return StateIncomplete
	}
}

// SeedLastRun initializes job name's snapshot from a previously persisted
// run (see the store package's GetLastRun), so a restart's web UI can still
// show when the job last ran instead of reverting to "never" until it next
// runs. Called once at startup, before any job's own goroutine can call
// starting/finished, so it never races an actual run.
func (s *StatusStore) SeedLastRun(name string, start, end time.Time, state RunState, errText string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.State = state
	j.LastStart = start
	j.LastEnd = end
	j.Duration = end.Sub(start).Round(time.Millisecond).String()
	j.Error = errText

	setSizeIfSucceeded(&j.Size, state, size)
}

// SeedTargetRun initializes job name's target whose server matches target
// from a previously persisted result (see store.ListTargetRuns), mirroring
// seedLastRun's reasoning one level down: a restart's web UI can still show
// each target's last outcome instead of every target reverting to "idle"
// until it next runs. Called once at startup, before any job's own goroutine
// can call starting/targetDone, so it never races an actual run.
func (s *StatusStore) SeedTargetRun(name, target string, state RunState, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	for i := range j.Targets {
		if j.Targets[i].Server == target {
			j.Targets[i].State = state
			j.Targets[i].Error = errText

			return
		}
	}
}

// RetryStarting marks name's targets (identified by TargetSnapshot.Server)
// as running, resetting each one's last error, the same way Starting does —
// but, unlike Starting, leaves every other target's already-recorded state
// untouched, since pipeline.Runner.RetryFailedTargets only re-runs this
// specific subset rather than the whole job. It still updates the job's own
// LastStart, so the dashboard's "last run" timestamp reflects this retry.
func (s *StatusStore) RetryStarting(name string, targets []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.LastStart = time.Now()

	for i := range j.Targets {
		if slices.Contains(targets, j.Targets[i].Server) {
			j.Targets[i].State = StateRunning
			j.Targets[i].Error = ""
		}
	}
}

// FailedTargets returns the server names of every target in name currently
// reported as failed, for the web UI's "retry failed targets" action (see
// handleRetryFailedTargets in webui.go) to determine which targets a retry
// request applies to. Returns nil if name is unknown or none are failed.
func (s *StatusStore) FailedTargets(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return nil
	}

	var out []string

	for _, t := range j.Targets {
		if t.State == StateFailed {
			out = append(out, t.Server)
		}
	}

	return out
}

// Snapshot returns every job's current status, in config-file order, each a
// copy safe to serialize (or otherwise use) without holding s.mu.
func (s *StatusStore) Snapshot() []JobSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]JobSnapshot, 0, len(s.order))

	for _, name := range s.order {
		j := *s.jobs[name]
		j.Targets = append([]TargetSnapshot(nil), j.Targets...)
		out = append(out, j)
	}

	return out
}
