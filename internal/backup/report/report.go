// Package report resolves and validates the config file's report: entry,
// the optional daily/periodic summary of backup activity sent to one or
// more configured notifications.
package report

import (
	"errors"
	"fmt"
	"strings"
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
	Enabled bool `yaml:"enabled"`

	// Schedule is a standard 5-field cron expression (minute hour
	// day-of-month month day-of-week), always evaluated in UTC regardless
	// of this process's local time zone, e.g. "0 7 * * *" for once a day at
	// 07:00 UTC, or "0 */6 * * *" for every 6h. Also accepts cron's
	// descriptor shorthands (e.g. "@daily", "@every 6h"). A CRON_TZ=/TZ=
	// prefix is rejected. Default "0 7 * * *" (once a day at 07:00 UTC).
	Schedule string `yaml:"schedule"`

	// Notifications names top-level notifications: entries (see
	// notify.Build) to send this report to. A notification's email: block,
	// if set, receives the full report as its body, with its own
	// subject:/subject-placeholders (see pipeline.renderReportSubject)
	// substituted in if set, otherwise defaulting to defaultReportSubject; a
	// notification's webhook: block, if set, receives a JSON summary (or
	// its own body: template rendered the same way). Required (non-empty)
	// when Enabled.
	Notifications []string `yaml:"notifications"`
}

// Settings is fileReport after validation, ready for
// pipeline.RunReportLoop to act on. Its zero value (Enabled false) means the
// report is disabled.
type Settings struct {
	Enabled bool

	// Schedule is fileReport.Schedule, parsed once here rather than
	// re-parsed on every scheduling loop iteration.
	Schedule cron.Schedule

	// Notifications is fileReport.Notifications resolved against the config
	// file's top-level notifications: (see notify.Build).
	Notifications []notify.Notification
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

// defaultReportSchedule is fileReport.Schedule's default when left unset:
// once a day at 07:00 UTC.
const defaultReportSchedule = "0 7 * * *"

// ResolveSettings validates cfg (the config file's report: entry) against
// notifications (the config file's already-resolved top-level
// notifications: map, see notify.Build) and resolves it into a Settings. An
// unset/false cfg.Enabled returns the zero value, leaving the report
// disabled.
func ResolveSettings(cfg FileReport, notifications map[string]notify.Notification) (Settings, error) {
	if !cfg.Enabled {
		return Settings{}, nil
	}

	if len(cfg.Notifications) == 0 {
		return Settings{}, errors.New("report.enabled is true but report.notifications is not set")
	}

	resolved := make([]notify.Notification, len(cfg.Notifications))

	for i, id := range cfg.Notifications {
		n, ok := notifications[id]
		if !ok {
			return Settings{}, fmt.Errorf("report.notifications[%d]: unknown notification id %q", i, id)
		}

		resolved[i] = n
	}

	schedule := strings.TrimSpace(cfg.Schedule)
	if schedule == "" {
		schedule = defaultReportSchedule
	}

	sched, err := parseUTCSchedule(schedule)
	if err != nil {
		return Settings{}, err
	}

	return Settings{
		Enabled:       true,
		Schedule:      sched,
		Notifications: resolved,
	}, nil
}
