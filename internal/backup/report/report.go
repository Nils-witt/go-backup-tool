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
// dashboard — but reads the same receiver_events/job_runs history (the
// store package) and on-disk receiver state (objectstore/receiverfiles.go)
// the dashboard itself uses. Unset (the default) disables it.
type FileReport struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Schedule is a standard 5-field cron expression (minute hour
	// day-of-month month day-of-week), always evaluated in UTC regardless
	// of this process's local time zone, e.g. "0 7 * * *" for once a day at
	// 07:00 UTC, or "0 */6 * * *" for every 6h. Also accepts cron's
	// descriptor shorthands (e.g. "@daily", "@every 6h"). A CRON_TZ=/TZ=
	// prefix is rejected. Default "0 7 * * *" (once a day at 07:00 UTC).
	Schedule string `yaml:"schedule" json:"schedule"`

	// Notifications names notifications (managed in the web UI, see the
	// settings package) to send this report to. A notification's email: block,
	// if set, receives the full report as its body, with its own
	// subject:/subject-placeholders (see pipeline.renderReportSubject)
	// substituted in if set, otherwise defaulting to defaultReportSubject; a
	// notification's webhook: block, if set, receives a JSON summary (or
	// its own body: template rendered the same way). Required (non-empty)
	// when Enabled.
	Notifications []string `yaml:"notifications" json:"notifications"`

	// HistoryDays is how many days a generated report (scheduled or made on
	// demand in the web UI) is kept for viewing in the web UI before it's
	// deleted. 0 (unset) means DefaultHistoryDays; at most MaxHistoryDays.
	// Applies even while Enabled is false, to reports made on demand.
	HistoryDays int `yaml:"history-days,omitempty" json:"history_days"`
}

// DefaultHistoryDays is FileReport.HistoryDays' default when left unset.
const DefaultHistoryDays = 30

// MaxHistoryDays bounds FileReport.HistoryDays (about ten years).
const MaxHistoryDays = 3650

// Settings is fileReport after validation, ready for
// reporter.Run to act on. Its zero value (Enabled false) means the
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

	// HistoryRetention is how long a generated report is kept (see
	// FileReport.HistoryDays). Set even while the report is disabled; use
	// Retention to read it, which also covers the zero value.
	HistoryRetention time.Duration
}

// Retention returns s.HistoryRetention, or DefaultHistoryDays when it's
// unset (as in the zero Settings of a report whose stored settings don't
// resolve).
func (s Settings) Retention() time.Duration {
	if s.HistoryRetention <= 0 {
		return DefaultHistoryDays * 24 * time.Hour
	}

	return s.HistoryRetention
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
// returns a disabled Settings carrying only the history retention, though
// the schedule is still checked so a bad one can't be saved for later.
func ResolveSettings(cfg FileReport, notifications *notify.Registry) (Settings, error) {
	schedule := strings.TrimSpace(cfg.Schedule)
	if schedule == "" {
		schedule = DefaultSchedule
	}

	sched, err := parseUTCSchedule(schedule)
	if err != nil {
		return Settings{}, err
	}

	if cfg.HistoryDays < 0 || cfg.HistoryDays > MaxHistoryDays {
		return Settings{}, fmt.Errorf("report.history-days must be between 0 and %d, got %d", MaxHistoryDays, cfg.HistoryDays)
	}

	history := cfg.HistoryDays
	if history == 0 {
		history = DefaultHistoryDays
	}

	retention := time.Duration(history) * 24 * time.Hour

	if notifications != nil {
		for i, id := range cfg.Notifications {
			if _, ok := notifications.Get(id); !ok {
				return Settings{}, fmt.Errorf("report.notifications[%d]: unknown notification id %q", i, id)
			}
		}
	}

	if !cfg.Enabled {
		return Settings{HistoryRetention: retention}, nil
	}

	if len(cfg.Notifications) == 0 {
		return Settings{}, errors.New("report.enabled is true but report.notifications is not set")
	}

	return Settings{
		Enabled:          true,
		Schedule:         sched,
		Notifications:    slices.Clone(cfg.Notifications),
		HistoryRetention: retention,
	}, nil
}

// Live holds the report's current Settings, replaced when the report is
// edited in the web UI. reporter.Run watches Changed to
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
