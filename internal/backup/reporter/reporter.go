// Package reporter builds and sends the periodic receiver/job report: it
// waits for the schedule in a report.Live, gathers receiver and job history
// from the state db and the receivers' on-disk state, and delivers it to the
// configured notifications. Package report holds the report's settings;
// this package does the sending.
package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/periodic"
	reportpkg "nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// Deps are what a Reporter reads each time it builds a report. Receivers
// (e.g. receiver.Registry.Snapshot), notifications (their live registry)
// and jobs (JobNames) are read at each report, so ones managed in the web
// UI are included. DB may be nil (the state db couldn't be opened at
// startup): reports are then still sent, just without any
// receiver_events/job_runs history (see buildReport), and none is kept.
type Deps struct {
	// ServerName is this instance's {server_name} (see
	// config.RunConfig.ServerName).
	ServerName    string
	JobNames      func() []string
	Settings      *reportpkg.Live
	Notifications *notify.Registry
	Receivers     func() map[string]config.ResolvedReceiver
	DB            *store.Store
	Queue         *notify.Queue
	Log           *slog.Logger
}

// Reporter builds the receiver/job report: on the schedule in its
// settings (see Run), or on demand (see Generate). Every report it builds
// is kept in the state db for the settings' history retention (see
// reportpkg.Settings.Retention), so the web UI can show past ones.
type Reporter struct {
	deps Deps
	log  *slog.Logger
}

// New returns a Reporter reading deps. It does nothing until Run.
func New(deps Deps) *Reporter {
	return &Reporter{deps: deps, log: deps.Log.With("component", "report")}
}

// ErrHistoryUnavailable is returned by Generate when there's no state db
// to keep the report in.
var ErrHistoryUnavailable = errors.New("state db unavailable: reports can't be kept this run")

// MaxGeneratePeriod bounds Generate's period: the receiver and job event
// logs it summarizes are pruned after 90 days anyway.
const MaxGeneratePeriod = 90 * 24 * time.Hour

// Run sends the receiver/job report on the schedule in the settings (see
// reportpkg.Settings), evaluated in UTC (see reportpkg.ResolveSettings),
// until ctx is done, idling while the report is disabled. A change to the
// settings (the report edited in the web UI) reschedules immediately.
// Alongside it, reports older than the history retention are deleted once
// at startup and then daily.
func (r *Reporter) Run(ctx context.Context) {
	if r.deps.DB != nil {
		go periodic.Run(ctx, historyPruneInterval, true, func() { r.pruneHistory(ctx) })
	}

	live := r.deps.Settings

	var prev time.Time // zero until the first report in this process has been sent

	for {
		changed := live.Changed()
		settings := live.Get()

		if !settings.Enabled {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}

		next := settings.Schedule.Next(time.Now().UTC())
		r.log.Debug("scheduled next report", "at", next)

		switch waitUntilOrChanged(ctx, next, changed) {
		case waitDone:
			return
		case waitChanged:
			continue
		case waitReached:
		}

		start := prev
		if start.IsZero() {
			start = next.Add(-24 * time.Hour)
		}

		report := r.build(ctx, start, next)
		r.keep(ctx, report, store.ReportTriggerScheduled, "", settings.Notifications)
		sendReport(ctx, report, r.deps.Notifications.Resolve(settings.Notifications, r.log), r.deps.Queue, r.log)

		prev = next
	}
}

// Generate builds a report covering the period up to now, without sending
// it anywhere, keeps it in the state db as made by user, and returns it as
// kept. period must be positive and at most MaxGeneratePeriod.
func (r *Reporter) Generate(ctx context.Context, user string, period time.Duration) (store.Report, error) {
	if r.deps.DB == nil {
		return store.Report{}, ErrHistoryUnavailable
	}

	if period <= 0 || period > MaxGeneratePeriod {
		return store.Report{}, fmt.Errorf("report period must be between 1h and %s, got %s", MaxGeneratePeriod, period)
	}

	end := time.Now().UTC()
	report := r.build(ctx, end.Add(-period), end)

	kept, err := r.deps.DB.SaveReport(ctx, storedReport(report, store.ReportTriggerManual, user, nil))
	if err != nil {
		return store.Report{}, err
	}

	r.log.Info("report generated on demand", "id", kept.ID, "period", period, "by", user)

	return kept, nil
}

// build builds the report for the window from start to end.
func (r *Reporter) build(ctx context.Context, start, end time.Time) reportContent {
	return buildReport(ctx, r.deps.ServerName, r.deps.JobNames(), r.deps.Receivers(), r.deps.DB, start, end, r.log)
}

// keep stores a scheduled report in the history, logging (rather than
// returning) a failure: the report is still sent.
func (r *Reporter) keep(ctx context.Context, report reportContent, trigger, user string, notifications []string) {
	if r.deps.DB == nil {
		return
	}

	if _, err := r.deps.DB.SaveReport(ctx, storedReport(report, trigger, user, notifications)); err != nil {
		r.log.Warn("keeping report in history failed", "err", err)
	}
}

// historyPruneInterval is how often Run deletes reports past the history
// retention.
const historyPruneInterval = 24 * time.Hour

// pruneHistory deletes every kept report older than the current history
// retention.
func (r *Reporter) pruneHistory(ctx context.Context) {
	retention := r.deps.Settings.Get().Retention()

	n, err := r.deps.DB.DeleteReportsBefore(ctx, time.Now().Add(-retention))
	if err != nil {
		r.log.Warn("pruning report history", "err", err)
		return
	}

	if n > 0 {
		r.log.Info("pruned old reports", "reports", n, "retention", retention)
	}
}

// storedReport converts report into the store's shape.
func storedReport(report reportContent, trigger, user string, notifications []string) store.Report {
	c := store.ReportContent{
		Receivers:      make([]store.ReportReceiverLine, len(report.receivers)),
		Stale:          make([]store.ReportStaleReceiver, len(report.stale)),
		ReceiverErrors: make([]store.ReportReceiverError, len(report.errors)),
		Jobs:           make([]store.ReportJobLine, len(report.jobs)),
		JobErrors:      make([]store.ReportJobError, len(report.jobErrors)),
	}

	for i, l := range report.receivers {
		c.Receivers[i] = store.ReportReceiverLine{ID: l.id, FilesReceived: l.filesReceived, BytesReceived: l.bytesReceived, Errors: l.errors}
	}

	for i, s := range report.stale {
		line := store.ReportStaleReceiver{ID: s.id, StaleAfterSeconds: int64(s.staleAfter / time.Second)}
		if !s.lastSeen.IsZero() {
			lastSeen := s.lastSeen.UTC()
			line.LastSeen = &lastSeen
		}

		c.Stale[i] = line
	}

	for i, e := range report.errors {
		c.ReceiverErrors[i] = store.ReportReceiverError{At: e.At.UTC(), ReceiverID: e.ReceiverID, Kind: e.Kind, Key: e.Key, Error: e.Error}
	}

	for i, l := range report.jobs {
		c.Jobs[i] = store.ReportJobLine{ID: l.id, RunsCompleted: l.runsCompleted, BytesWritten: l.bytesWritten, Errors: l.errors}
	}

	for i, e := range report.jobErrors {
		c.JobErrors[i] = store.ReportJobError{At: e.At.UTC(), JobName: e.JobName, Error: e.Error}
	}

	return store.Report{
		GeneratedAt: time.Now().UTC(), PeriodStart: report.start, PeriodEnd: report.end,
		Trigger: trigger, TriggeredBy: user, Notifications: notifications,
		Content: c,
	}
}

// waitResult is how waitUntilOrChanged returned.
type waitResult int

const (
	waitReached waitResult = iota // the target time arrived
	waitChanged                   // changed was closed first
	waitDone                      // ctx was done first
)

// waitUntilOrChanged blocks until t, until changed is closed, or until ctx
// is done, whichever comes first.
func waitUntilOrChanged(ctx context.Context, t time.Time, changed <-chan struct{}) waitResult {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return waitDone
	case <-changed:
		return waitChanged
	case <-timer.C:
		return waitReached
	}
}

// receiverReportLine is one configured receiver's activity over a
// reportContent's window, in the order its config file entry was listed.
type receiverReportLine struct {
	id            string
	filesReceived int
	bytesReceived int64
	errors        int
}

// staleReceiverLine is one receiver found currently stale (see
// annotateReceiverStaleness's identical condition) when a reportContent was
// built.
type staleReceiverLine struct {
	id         string
	staleAfter time.Duration
	lastSeen   time.Time // zero if it has never received anything at all
}

// jobReportLine is one configured job's activity over a reportContent's
// window, in the order its config file entry was listed — mirroring
// receiverReportLine.
type jobReportLine struct {
	id            string
	runsCompleted int
	bytesWritten  int64
	errors        int
}

// reportContent is the computed content of one report email, built by
// buildReport and rendered to a message body by renderReportBody.
type reportContent struct {
	start, end time.Time
	receivers  []receiverReportLine
	errors     []store.ReceiverErrorEvent
	stale      []staleReceiverLine
	jobs       []jobReportLine
	jobErrors  []store.JobRunErrorEvent

	// serverName is rc.ServerName (see config.RunConfig.ServerName), copied
	// in by buildReport so renderReportSubject/reportWebhookBody can
	// substitute it into a report notification's {server_name} placeholder.
	serverName string
}

// buildReport summarizes rc's configured receivers' and jobs' activity in
// the window from start to end: files received/runs completed and errors
// from receiver_events/job_runs (db, skipped if nil), and current receiver
// staleness read live from disk (see objectstore.LastReceivedAt), mirroring the
// dashboard's own annotateReceiverStaleness so the two never disagree. A
// query failure is logged and leaves that section empty rather than failing
// the whole report — a partial report is better than none, matching this
// codebase's usual failure handling.
func buildReport(ctx context.Context, serverName string, jobNames []string, receivers map[string]config.ResolvedReceiver, db *store.Store, start, end time.Time, log *slog.Logger) reportContent {
	report := reportContent{start: start, end: end, serverName: serverName}

	report.receivers, report.stale, report.errors = buildReceiverReport(ctx, receivers, db, start, end, log)
	report.jobs, report.jobErrors = buildJobReport(ctx, jobNames, db, start, end, log)

	return report
}

// buildReceiverReport summarizes receivers' activity in the
// window from start to end: files received and errors from receiver_events
// (db, skipped if nil), and current staleness read live from disk (see
// objectstore.LastReceivedAt), mirroring the dashboard's own
// annotateReceiverStaleness so the two never disagree. A query failure is
// logged and leaves that section empty rather than failing the whole
// report — a partial report is better than none, matching this codebase's
// usual failure handling.
func buildReceiverReport(ctx context.Context, receivers map[string]config.ResolvedReceiver, db *store.Store, start, end time.Time, log *slog.Logger) ([]receiverReportLine, []staleReceiverLine, []store.ReceiverErrorEvent) {
	ids := make([]string, 0, len(receivers))
	for id := range receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	byID := make(map[string]store.ReceiverDaySummary, len(ids))

	var errs []store.ReceiverErrorEvent

	if db != nil {
		summaries, err := db.SummarizeReceiverEvents(ctx, start, end)
		if err != nil {
			log.Warn("daily report: summarizing receiver events failed", "err", err)
		}

		for _, s := range summaries {
			byID[s.ReceiverID] = s
		}

		errs, err = db.ListReceiverErrorEvents(ctx, start, end)
		if err != nil {
			log.Warn("daily report: reading receiver error events failed", "err", err)

			errs = nil
		}
	}

	var lines []receiverReportLine

	var stale []staleReceiverLine

	for _, id := range ids {
		s := byID[id]
		lines = append(lines, receiverReportLine{
			id: id, filesReceived: s.FilesReceived, bytesReceived: s.BytesReceived, errors: s.Errors,
		})

		recv := receivers[id]
		if recv.StaleAfter <= 0 {
			continue
		}

		lastSeen, ok, err := objectstore.LastReceivedAt(recv)
		if err != nil {
			log.Warn("daily report: checking receiver staleness failed", "id", id, "err", err)
			continue
		}

		if ok && time.Since(lastSeen) > recv.StaleAfter {
			stale = append(stale, staleReceiverLine{id: id, staleAfter: recv.StaleAfter, lastSeen: lastSeen})
		}
	}

	return lines, stale, errs
}

// buildJobReport summarizes jobNames' activity in the window
// from start to end: runs completed and errors from job_runs (db, skipped
// if nil), mirroring buildReceiverReport. A query failure is logged and
// leaves that section empty rather than failing the whole report.
func buildJobReport(ctx context.Context, jobNames []string, db *store.Store, start, end time.Time, log *slog.Logger) ([]jobReportLine, []store.JobRunErrorEvent) {
	ids := slices.Sorted(slices.Values(jobNames))

	byID := make(map[string]store.JobRunDaySummary, len(ids))

	var errs []store.JobRunErrorEvent

	if db != nil {
		summaries, err := db.SummarizeJobRuns(ctx, start, end)
		if err != nil {
			log.Warn("daily report: summarizing job runs failed", "err", err)
		}

		for _, s := range summaries {
			byID[s.JobName] = s
		}

		errs, err = db.ListJobRunErrorEvents(ctx, start, end)
		if err != nil {
			log.Warn("daily report: reading job run error events failed", "err", err)

			errs = nil
		}
	}

	var jobs []jobReportLine

	for _, id := range ids {
		s := byID[id]
		jobs = append(jobs, jobReportLine{
			id: id, runsCompleted: s.RunsCompleted, bytesWritten: s.BytesWritten, errors: s.Errors,
		})
	}

	return jobs, errs
}

// renderReportBody renders report as a plain-text email body.
func renderReportBody(report reportContent) string {
	var b strings.Builder

	fmt.Fprintf(&b, "go-backup-tool report\n")
	fmt.Fprintf(&b, "Period: %s to %s (UTC)\n\n", report.start.UTC().Format(time.RFC3339), report.end.UTC().Format(time.RFC3339))

	if len(report.receivers) == 0 {
		b.WriteString("No receivers configured.\n\n")
	} else {
		b.WriteString("Files received per receiver:\n")

		for _, r := range report.receivers {
			fmt.Fprintf(&b, "  %-24s %5d file(s), %10s, %d error(s)\n", r.id, r.filesReceived, formatReportBytes(r.bytesReceived), r.errors)
		}

		b.WriteString("\n")
	}

	if len(report.stale) == 0 {
		b.WriteString("No receivers currently stale.\n\n")
	} else {
		b.WriteString("Stale receivers:\n")

		for _, s := range report.stale {
			lastSeen := "never"
			if !s.lastSeen.IsZero() {
				lastSeen = s.lastSeen.UTC().Format(time.RFC3339)
			}

			fmt.Fprintf(&b, "  %s: last received %s (stale-after: %s)\n", s.id, lastSeen, s.staleAfter)
		}

		b.WriteString("\n")
	}

	if len(report.errors) == 0 {
		b.WriteString("No receiver errors recorded.\n\n")
	} else {
		b.WriteString("Receiver errors:\n")

		for _, e := range report.errors {
			fmt.Fprintf(&b, "  [%s] %s %s %q: %s\n", e.At.UTC().Format(time.RFC3339), e.ReceiverID, e.Kind, e.Key, e.Error)
		}

		b.WriteString("\n")
	}

	if len(report.jobs) == 0 {
		b.WriteString("No jobs configured.\n\n")
	} else {
		b.WriteString("Runs completed per job:\n")

		for _, j := range report.jobs {
			fmt.Fprintf(&b, "  %-24s %5d run(s), %10s, %d error(s)\n", j.id, j.runsCompleted, formatReportBytes(j.bytesWritten), j.errors)
		}

		b.WriteString("\n")
	}

	if len(report.jobErrors) == 0 {
		b.WriteString("No job errors recorded.\n")
	} else {
		b.WriteString("Job errors:\n")

		for _, e := range report.jobErrors {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", e.At.UTC().Format(time.RFC3339), e.JobName, e.Error)
		}
	}

	return b.String()
}

// reportTimeFormat is how {start}/{end} render (in UTC) in a notification's
// subject:/body: template (see renderReportSubject), matching this
// feature's original hardcoded subject line.
const reportTimeFormat = "2006-01-02 15:04"

// defaultReportSubject is a report notification's email.subject default
// when left unset, reproducing this feature's original hardcoded subject
// line.
const defaultReportSubject = "[{server_name}] report - {end}"

// renderReportSubject substitutes a report notification's subject:/body:
// template's placeholders with report's computed content: {start} and {end}
// are report's window bounds; {receivers}, {errors}, {stale}, {jobs}, and
// {job-errors} are counts of each of report's sections; {server_name} is the
// config file's top-level server-name: (see config.RunConfig.ServerName),
// "" if unset. Mirrors receiver.renderDownloadWebhookPayload's
// {placeholder} substitution style. {server_name}, the only free-text
// value, goes through escape first (see notify.Webhook.TemplateEscaper).
func renderReportSubject(tmpl string, report reportContent, escape func(string) string) string {
	replacer := strings.NewReplacer(
		"{start}", report.start.UTC().Format(reportTimeFormat),
		"{end}", report.end.UTC().Format(reportTimeFormat),
		"{receivers}", strconv.Itoa(len(report.receivers)),
		"{errors}", strconv.Itoa(len(report.errors)),
		"{stale}", strconv.Itoa(len(report.stale)),
		"{jobs}", strconv.Itoa(len(report.jobs)),
		"{job-errors}", strconv.Itoa(len(report.jobErrors)),
		"{server_name}", escape(report.serverName),
	)

	return replacer.Replace(tmpl)
}

// formatReportBytes formats n bytes as a short human-readable size (e.g.
// "1.2 GB") for the report body. Unlike a general-purpose humanize package,
// this only needs to read reasonably in an email, not be exact.
func formatReportBytes(n int64) string {
	return status.FormatSize(n, 1000, "kMGTPE", false)
}

// reportNotifyTimeout bounds a single report notification delivery (one
// email send or one webhook POST), since sendReport runs on its own
// background schedule rather than under a run's -timeout.
const reportNotifyTimeout = 30 * time.Second

// reportWebhookPayload is the default JSON body POSTed to a report
// notification's webhook:, used unless its body: overrides it (rendered the
// same way as its subject:, via renderReportSubject).
type reportWebhookPayload struct {
	Start      string `json:"start"`
	End        string `json:"end"`
	Receivers  int    `json:"receivers"`
	Errors     int    `json:"errors"`
	Stale      int    `json:"stale"`
	Jobs       int    `json:"jobs"`
	JobErrors  int    `json:"job_errors"`
	ServerName string `json:"server_name,omitempty"`
}

// defaultReportWebhookContentType is the Content-Type sent with a report
// webhook request whose notification doesn't set a Content-Type among
// webhook.headers:.
const defaultReportWebhookContentType = "application/json"

// reportWebhookBody builds the request body sent to a report notification's
// webhook: wh.Body: (rendered via renderReportSubject), if set, otherwise
// the default JSON reportWebhookPayload.
func reportWebhookBody(wh notify.Webhook, report reportContent) ([]byte, error) {
	if wh.Body != "" {
		esc := wh.TemplateEscaper(defaultReportWebhookContentType)

		return []byte(renderReportSubject(wh.Body, report, esc)), nil
	}

	payload := reportWebhookPayload{
		Start: report.start.UTC().Format(reportTimeFormat), End: report.end.UTC().Format(reportTimeFormat),
		Receivers: len(report.receivers), Errors: len(report.errors), Stale: len(report.stale),
		Jobs: len(report.jobs), JobErrors: len(report.jobErrors),
		ServerName: report.serverName,
	}

	return json.Marshal(payload)
}

// sendReport sends report to every one of notifications, logging (rather
// than returning) any failure: like a receiver's stale/download
// notifications, a delivery problem here shouldn't affect anything else this
// process is doing, and there's no caller to report it to — the next
// scheduled report gets another chance.
func sendReport(ctx context.Context, report reportContent, notifications []notify.Notification, queue *notify.Queue, log *slog.Logger) {
	body := renderReportBody(report)

	for _, n := range notifications {
		if n.Email != nil {
			sendReportEmail(ctx, n.Email, report, body, queue, log)
		}

		if n.Webhook != nil {
			sendReportWebhook(ctx, n.Webhook, report, log)
		}
	}
}

// sendReportEmail sends report's rendered body (subject via
// renderReportSubject, defaulting to defaultReportSubject) to email. queue,
// if non-nil, retries delivery later on failure instead of losing it.
func sendReportEmail(ctx context.Context, email *notify.Email, report reportContent, body string, queue *notify.Queue, log *slog.Logger) {
	subjectTmpl := email.Subject
	if subjectTmpl == "" {
		subjectTmpl = defaultReportSubject
	}

	subject := renderReportSubject(subjectTmpl, report, notify.NoEscape)

	sendCtx, cancel := context.WithTimeout(ctx, reportNotifyTimeout)
	defer cancel()

	if err := notify.SendMailQueued(sendCtx, queue, email.SMTP, email.From, email.To, subject, body, email.Encrypt); err != nil {
		log.Warn("report: sending email failed, queued for retry", "to", email.To, "err", err)
		return
	}

	log.Info("report email sent", "to", email.To,
		"receivers", len(report.receivers), "errors", len(report.errors), "stale", len(report.stale),
		"jobs", len(report.jobs), "jobErrors", len(report.jobErrors))
}

// sendReportWebhook POSTs report to wh.
func sendReportWebhook(ctx context.Context, wh *notify.Webhook, report reportContent, log *slog.Logger) {
	body, err := reportWebhookBody(*wh, report)
	if err != nil {
		log.Warn("report: encoding webhook payload failed", "webhook", wh.RedactedURL(), "err", err)
		return
	}

	resp, err := notify.PostWebhook(ctx, *wh, body, defaultReportWebhookContentType)
	if err != nil {
		log.Warn("report: webhook request failed", "webhook", wh.RedactedURL(), "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("report: webhook non-2xx response", "webhook", wh.RedactedURL(), "status", resp.StatusCode)
		return
	}

	log.Info("report webhook fired", "webhook", wh.RedactedURL(),
		"receivers", len(report.receivers), "errors", len(report.errors), "stale", len(report.stale),
		"jobs", len(report.jobs), "jobErrors", len(report.jobErrors))
}
