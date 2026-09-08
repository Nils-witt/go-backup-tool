package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// RunReportLoop sends rc's receiver/job report on rc.Report's configured
// cron schedule, in this process's local time zone, until ctx is done. A
// no-op if the report isn't enabled. db may be nil (the state db couldn't be
// opened at startup); the report is still sent, just without any
// receiver_events/job_runs history (see buildReport).
func RunReportLoop(ctx context.Context, rc *config.RunConfig, db *store.Store, queue *notify.Queue, log *slog.Logger) {
	if !rc.Report.Enabled {
		return
	}

	log = log.With("component", "report")

	var prev time.Time // zero until the first report in this process has been sent

	for {
		next := rc.Report.Schedule.Next(time.Now())
		log.Debug("scheduled next report", "at", next)

		if !waitUntil(ctx, next) {
			return
		}

		start := prev
		if start.IsZero() {
			start = next.Add(-24 * time.Hour)
		}

		sendReport(ctx, rc, db, start, next, queue, log)
		prev = next
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
// staleness read live from disk (see backup.LastReceivedAt), mirroring the
// dashboard's own annotateReceiverStaleness so the two never disagree. A
// query failure is logged and leaves that section empty rather than failing
// the whole report — a partial report is better than none, matching this
// codebase's usual failure handling.
func buildReport(ctx context.Context, rc *config.RunConfig, db *store.Store, start, end time.Time, log *slog.Logger) reportContent {
	report := reportContent{start: start, end: end, serverName: rc.ServerName}

	report.receivers, report.stale, report.errors = buildReceiverReport(ctx, rc, db, start, end, log)
	report.jobs, report.jobErrors = buildJobReport(ctx, rc, db, start, end, log)

	return report
}

// buildReceiverReport summarizes rc's configured receivers' activity in the
// window from start to end: files received and errors from receiver_events
// (db, skipped if nil), and current staleness read live from disk (see
// backup.LastReceivedAt), mirroring the dashboard's own
// annotateReceiverStaleness so the two never disagree. A query failure is
// logged and leaves that section empty rather than failing the whole
// report — a partial report is better than none, matching this codebase's
// usual failure handling.
func buildReceiverReport(ctx context.Context, rc *config.RunConfig, db *store.Store, start, end time.Time, log *slog.Logger) ([]receiverReportLine, []staleReceiverLine, []store.ReceiverErrorEvent) {
	ids := make([]string, 0, len(rc.Receivers))
	for id := range rc.Receivers {
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

	var receivers []receiverReportLine

	var stale []staleReceiverLine

	for _, id := range ids {
		s := byID[id]
		receivers = append(receivers, receiverReportLine{
			id: id, filesReceived: s.FilesReceived, bytesReceived: s.BytesReceived, errors: s.Errors,
		})

		recv := rc.Receivers[id]
		if recv.StaleAfter <= 0 {
			continue
		}

		lastSeen, ok, err := backup.LastReceivedAt(recv)
		if err != nil {
			log.Warn("daily report: checking receiver staleness failed", "id", id, "err", err)
			continue
		}

		if ok && time.Since(lastSeen) > recv.StaleAfter {
			stale = append(stale, staleReceiverLine{id: id, staleAfter: recv.StaleAfter, lastSeen: lastSeen})
		}
	}

	return receivers, stale, errs
}

// buildJobReport summarizes rc's configured jobs' activity in the window
// from start to end: runs completed and errors from job_runs (db, skipped
// if nil), mirroring buildReceiverReport. A query failure is logged and
// leaves that section empty rather than failing the whole report.
func buildJobReport(ctx context.Context, rc *config.RunConfig, db *store.Store, start, end time.Time, log *slog.Logger) ([]jobReportLine, []store.JobRunErrorEvent) {
	ids := make([]string, 0, len(rc.Jobs))
	for _, job := range rc.Jobs {
		ids = append(ids, job.Name)
	}

	sort.Strings(ids)

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

// reportTimeFormat is how {start}/{end} render in a notification's
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
// {placeholder} substitution style.
func renderReportSubject(tmpl string, report reportContent) string {
	replacer := strings.NewReplacer(
		"{start}", report.start.Format(reportTimeFormat),
		"{end}", report.end.Format(reportTimeFormat),
		"{receivers}", strconv.Itoa(len(report.receivers)),
		"{errors}", strconv.Itoa(len(report.errors)),
		"{stale}", strconv.Itoa(len(report.stale)),
		"{jobs}", strconv.Itoa(len(report.jobs)),
		"{job-errors}", strconv.Itoa(len(report.jobErrors)),
		"{server_name}", report.serverName,
	)

	return replacer.Replace(tmpl)
}

// formatReportBytes formats n bytes as a short human-readable size (e.g.
// "1.2 GB") for the report body. Unlike a general-purpose humanize package,
// this only needs to read reasonably in an email, not be exact.
func formatReportBytes(n int64) string {
	return backup.FormatSize(n, 1000, "kMGTPE", false)
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
		return []byte(renderReportSubject(wh.Body, report)), nil
	}

	payload := reportWebhookPayload{
		Start: report.start.Format(reportTimeFormat), End: report.end.Format(reportTimeFormat),
		Receivers: len(report.receivers), Errors: len(report.errors), Stale: len(report.stale),
		Jobs: len(report.jobs), JobErrors: len(report.jobErrors),
		ServerName: report.serverName,
	}

	return json.Marshal(payload)
}

// sendReport builds rc's receiver/job report for the window from start to
// end and sends it to every notification in rc.Report.Notifications,
// logging (rather than returning) any failure: like a receiver's stale/
// download notifications, a delivery problem here shouldn't affect anything
// else this process is doing, and there's no caller to report it to — the
// next scheduled report gets another chance.
func sendReport(ctx context.Context, rc *config.RunConfig, db *store.Store, start, end time.Time, queue *notify.Queue, log *slog.Logger) {
	report := buildReport(ctx, rc, db, start, end, log)
	body := renderReportBody(report)

	for _, n := range rc.Report.Notifications {
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

	subject := renderReportSubject(subjectTmpl, report)

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
		log.Warn("report: encoding webhook payload failed", "webhook", wh.URL, "err", err)
		return
	}

	resp, err := notify.PostWebhook(ctx, *wh, body, defaultReportWebhookContentType)
	if err != nil {
		log.Warn("report: webhook request failed", "webhook", wh.URL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("report: webhook non-2xx response", "webhook", wh.URL, "status", resp.StatusCode)
		return
	}

	log.Info("report webhook fired", "webhook", wh.URL,
		"receivers", len(report.receivers), "errors", len(report.errors), "stale", len(report.stale),
		"jobs", len(report.jobs), "jobErrors", len(report.jobErrors))
}
