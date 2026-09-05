package pipeline

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"sort"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// RunReportLoop sends rc's receiver/job report on rc.Report's configured
// cron schedule, in this process's local time zone, until ctx is done. A
// no-op if the report isn't enabled. db may be nil (the state db couldn't be
// opened at startup); the report is still sent, just without any
// receiver_events/job_runs history (see buildReport).
func RunReportLoop(ctx context.Context, rc *config.RunConfig, db *store.Store, log *slog.Logger) {
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

		sendReport(ctx, rc, db, start, next, log)
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
	report := reportContent{start: start, end: end}

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

// formatReportBytes formats n bytes as a short human-readable size (e.g.
// "1.2 GB") for the report body. Unlike a general-purpose humanize package,
// this only needs to read reasonably in an email, not be exact.
func formatReportBytes(n int64) string {
	return backup.FormatSize(n, 1000, "kMGTPE", false)
}

// reportSMTPTimeout bounds the whole report email send (connect,
// authenticate, and deliver), since sendReport runs on its own background
// schedule rather than under a run's -timeout.
const reportSMTPTimeout = 30 * time.Second

// sendReport builds and emails rc's receiver report for the window from
// start to end, logging (rather than returning) any failure: like the
// stale-receiver webhook, a delivery problem here shouldn't affect anything
// else this process is doing, and there's no caller to report it to — the
// next scheduled report gets another chance.
func sendReport(ctx context.Context, rc *config.RunConfig, db *store.Store, start, end time.Time, log *slog.Logger) {
	report := buildReport(ctx, rc, db, start, end, log)

	subject := "go-backup-tool report - " + end.Format("2006-01-02 15:04")
	body := renderReportBody(report)

	sendCtx, cancel := context.WithTimeout(ctx, reportSMTPTimeout)
	defer cancel()

	if err := sendMail(sendCtx, rc.Report.SMTP, rc.Report.From, rc.Report.To, subject, body); err != nil {
		log.Warn("report: sending email failed", "err", err)
		return
	}

	log.Info("report sent", "to", rc.Report.To,
		"receivers", len(report.receivers), "errors", len(report.errors), "stale", len(report.stale),
		"jobs", len(report.jobs), "jobErrors", len(report.jobErrors))
}

// dialSMTP connects to cfg's mail server and returns a ready-to-use
// *smtp.Client: already TLS-wrapped for backup.SMTPSecurityTLS, or with
// STARTTLS already negotiated for backup.SMTPSecurityStartTLS. ctx's
// deadline (see reportSMTPTimeout) is applied directly to the underlying
// connection, since net/smtp's own operations aren't otherwise
// context-aware.
func dialSMTP(ctx context.Context, cfg report.SMTPSettings) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %q: %w", addr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	var conn net.Conn

	if cfg.Security == report.SMTPSecurityTLS {
		conn = tls.Client(rawConn, &tls.Config{ServerName: cfg.Host})
	} else {
		conn = rawConn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("initializing smtp client: %w", err)
	}

	if cfg.Security != report.SMTPSecurityStartTLS {
		return client, nil
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("starting tls: %w", err)
		}
	}

	return client, nil
}

// sendMail sends a plain-text email from sender to every address in
// recipients, via cfg.
func sendMail(ctx context.Context, cfg report.SMTPSettings, sender string, recipients []string, subject, body string) error {
	client, err := dialSMTP(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("authenticating: %w", err)
		}
	}

	if err := client.Mail(sender); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}

	for _, addr := range recipients {
		if err := client.Rcpt(addr); err != nil {
			return fmt.Errorf("RCPT TO %q: %w", addr, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}

	if _, err := w.Write([]byte(renderMailMessage(sender, recipients, subject, body))); err != nil {
		_ = w.Close()
		return fmt.Errorf("writing message: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("closing message: %w", err)
	}

	return client.Quit()
}

// renderMailMessage builds an RFC 5322 message (headers plus body) for
// sendMail's DATA command. Header values are config-file-controlled, not
// network input, but CRLF is still stripped defensively so a stray newline
// in report.from/to/subject can never inject an extra header or start of
// body.
func renderMailMessage(from string, to []string, subject, body string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\r\n", stripCRLF(from))
	fmt.Fprintf(&b, "To: %s\r\n", stripCRLF(strings.Join(to, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", stripCRLF(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))

	return b.String()
}

// stripCRLF removes CR and LF from s, for a value about to be written into
// an email header (see renderMailMessage).
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
