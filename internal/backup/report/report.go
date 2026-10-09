// Package report resolves and validates the config file's report: entry,
// the optional daily/periodic summary of backup activity sent to one or
// more configured notifications.
package report

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// FileReport is the top-level report: entry, configuring an optional
// summary: an overview of how many files each configured receiver received,
// any receiver API errors, any receiver currently stale, how many runs each
// configured job completed, and any job errors, sent to one or more
// top-level notifications: entries on a cron schedule. It's independent of
// the web UI dashboard (webui.go) — useful for anyone monitoring
// receivers/jobs by inbox or webhook, not just those watching the
// dashboard — but reads the same receiver_events/job_runs history
// (schedule_state.go) and on-disk receiver state (receiver.go) the
// dashboard itself uses. Unset (the default) disables it.
type FileReport struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Schedule is a standard 5-field cron expression (minute hour
	// day-of-month month day-of-week), always evaluated in UTC regardless
	// of this process's local time zone, e.g. "0 7 * * *" for once a day at
	// 07:00 UTC, or "0 */6 * * *" for every 6h. Also accepts cron's
	// descriptor shorthands (e.g. "@daily", "@every 6h"). A CRON_TZ=/TZ=
	// prefix is rejected. Default "0 7 * * *" (once a day at 07:00 UTC).
	Schedule string `yaml:"schedule" json:"schedule"`

	// Notifications names top-level notifications: entries (see
	// notify.Build) to send this report to. A notification's email: block,
	// if set, receives the full report as its body, with its own
	// subject:/subject-placeholders (see pipeline.renderReportSubject)
	// substituted in if set, otherwise defaulting to defaultReportSubject; a
	// notification's webhook: block, if set, receives a JSON summary (or
	// its own body: template rendered the same way). Required (non-empty)
	// when Enabled.
	Notifications []string `yaml:"notifications" json:"notifications"`
}

// Settings is fileReport after validation, ready for
// pipeline.RunReportLoop to act on. Its zero value (Enabled false) means the
// report is disabled.
type Settings struct {
	Enabled bool

	// Schedule is fileReport.Schedule, parsed once here rather than
	// re-parsed on every scheduling loop iteration.
	Schedule cron.Schedule

	// Notifications is fileReport.Notifications: notification ids, looked
	// up in the live notify.Registry each time the report is sent (see
	// notify.Registry.Resolve), so edits made in the web UI apply.
	Notifications []string
}

// parseUTCSchedule parses schedule as a standard cron expression (or
// descriptor) evaluated in UTC, regardless of this process's local time
// zone. A CRON_TZ=/TZ= prefix is rejected rather than honored: every
// schedule go-backup-tool runs is UTC-based.
func parseUTCSchedule(schedule string) (cron.Schedule, error) {
	if strings.HasPrefix(schedule, "CRON_TZ=") || strings.HasPrefix(schedule, "TZ=") {
		return nil, fmt.Errorf("report.schedule %q: time zone prefixes are not supported, schedules are always evaluated in UTC", schedule)
	}

	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return nil, fmt.Errorf("parsing report.schedule %q: %w (want a standard 5-field cron expression, e.g. \"0 7 * * *\")", schedule, err)
	}

	// ParseStandard leaves a spec schedule in time.Local; pin it to UTC.
	// An "@every" ConstantDelaySchedule has no time zone to pin.
	if spec, ok := sched.(*cron.SpecSchedule); ok {
		spec.Location = time.UTC
	}

	return sched, nil
}

// DefaultSchedule is fileReport.Schedule's default when left unset:
// once a day at 07:00 UTC.
const DefaultSchedule = "0 7 * * *"

// ResolveSettings validates cfg (a report definition, from the config
// file's report: or from the state db, managed in the web UI) and resolves
// it into a Settings, checking every notification id exists in
// notifications — skipped when notifications is nil, as when parsing the
// config file, before the live registry exists. An unset/false cfg.Enabled
// returns the zero value, leaving the report disabled, though the schedule
// is still checked so a bad one can't be saved for later.
func ResolveSettings(cfg FileReport, notifications *notify.Registry) (Settings, error) {
	schedule := strings.TrimSpace(cfg.Schedule)
	if schedule == "" {
		schedule = DefaultSchedule
	}

	sched, err := parseUTCSchedule(schedule)
	if err != nil {
		return Settings{}, err
	}

	if notifications != nil {
		for i, id := range cfg.Notifications {
			if _, ok := notifications.Get(id); !ok {
				return Settings{}, fmt.Errorf("report.notifications[%d]: unknown notification id %q", i, id)
			}
		}
	}

	if !cfg.Enabled {
		return Settings{}, nil
	}

	if len(cfg.Notifications) == 0 {
		return Settings{}, errors.New("report.enabled is true but report.notifications is not set")
	}

	return Settings{
		Enabled:       true,
		Schedule:      sched,
		Notifications: slices.Clone(cfg.Notifications),
	}, nil
}

// Live holds the report's current Settings, replaced when the report is
// edited in the web UI. pipeline.RunReportLoop watches Changed to
// reschedule without a restart. Safe for concurrent use.
type Live struct {
	mu       sync.Mutex
	settings Settings
	changed  chan struct{}
}

// NewLive returns a Live holding settings.
func NewLive(settings Settings) *Live {
	return &Live{settings: settings, changed: make(chan struct{})}
}

// Get returns the current settings.
func (l *Live) Get() Settings {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.settings
}

// Set replaces the current settings, waking every Changed waiter.
func (l *Live) Set(settings Settings) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.settings = settings
	close(l.changed)
	l.changed = make(chan struct{})
}

// Changed returns a channel closed on the next Set.
func (l *Live) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.changed
}
