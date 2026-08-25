package backup

import (
	"sync"
	"time"
)

// runState is the lifecycle state of a job or target as shown in the web UI
// (see webui.go).
type runState string

const (
	stateIdle    runState = "idle"    // configured, never run yet (or waiting for its next interval)
	stateRunning runState = "running" // currently executing
	stateOK      runState = "ok"      // last run succeeded
	stateFailed  runState = "failed"  // last run failed
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
	Duration  string           `json:"duration,omitempty"`
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

		s.jobs[j.name] = &jobSnapshot{Name: j.name, Interval: intervalString(j.interval), State: stateIdle, Targets: targets}
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

// finished records job name's overall outcome and duration once its run
// completes.
func (s *statusStore) finished(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	j, ok := s.jobs[name]
	if !ok {
		return
	}

	j.LastEnd = time.Now()
	j.Duration = j.LastEnd.Sub(j.LastStart).Round(time.Millisecond).String()

	if err != nil {
		j.State = stateFailed
		j.Error = err.Error()

		return
	}

	j.State = stateOK
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
