package pipeline

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"sort"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
)

// RunDailyReportLoop sends rc's daily receiver report once a day at
// rc.Report's configured time, in this process's local time zone, until ctx
// is done. A no-op if the daily report isn't enabled. db may be nil (the
// state db couldn't be opened at startup); the report is still sent, just
// without any receiver_events history (see buildDailyReport).
func RunDailyReportLoop(ctx context.Context, rc *backup.RunConfig, db *sql.DB, log *slog.Logger) {
	if !rc.Report.Enabled {
		return
	}

	log = log.With("component", "daily-report")

	for {
		next := nextDailyReportTime(rc.Report.SendHour, rc.Report.SendMinute, time.Now())
		log.Debug("scheduled next daily report", "at", next)

		if !waitUntil(ctx, next) {
			return
		}

		sendDailyReport(ctx, rc, db, next, log)
	}
}

// nextDailyReportTime returns the next time hour:minute occurs at or after
// now, in now's own location: today if that time hasn't passed yet,
// tomorrow otherwise.
func nextDailyReportTime(hour, minute int, now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}

	return next
}

// receiverReportLine is one configured receiver's activity over a
// dailyReport's window, in the order its config file entry was listed.
type receiverReportLine struct {
	id            string
	filesReceived int
	bytesReceived int64
	errors        int
}

// staleReceiverLine is one receiver found currently stale (see
// annotateReceiverStaleness's identical condition) when a dailyReport was
// built.
type staleReceiverLine struct {
	id         string
	staleAfter time.Duration
	lastSeen   time.Time // zero if it has never received anything at all
}

// dailyReport is the computed content of one daily report email, built by
// buildDailyReport and rendered to a message body by renderDailyReportBody.
type dailyReport struct {
	start, end time.Time
	receivers  []receiverReportLine
	errors     []backup.ReceiverErrorEvent
	stale      []staleReceiverLine
}

// buildDailyReport summarizes rc's configured receivers' activity in the
// 24h window ending at end: files received and errors from receiver_events
// (db, skipped if nil), and current staleness read live from disk (see
// backup.LastReceivedAt), mirroring the dashboard's own
// annotateReceiverStaleness so the two never disagree. A query failure is
// logged and leaves that section empty rather than failing the whole
// report — a partial report is better than none, matching this codebase's
// usual failure handling.
func buildDailyReport(ctx context.Context, rc *backup.RunConfig, db *sql.DB, end time.Time, log *slog.Logger) dailyReport {
	start := end.Add(-24 * time.Hour)
	report := dailyReport{start: start, end: end}

	ids := make([]string, 0, len(rc.Receivers))
	for id := range rc.Receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	byID := make(map[string]backup.ReceiverDaySummary, len(ids))

	if db != nil {
		summaries, err := backup.SummarizeReceiverEvents(ctx, db, start, end)
		if err != nil {
			log.Warn("daily report: summarizing receiver events failed", "err", err)
		}

		for _, s := range summaries {
			byID[s.ReceiverID] = s
		}

		errs, err := backup.ReadReceiverErrorEvents(ctx, db, start, end)
		if err != nil {
			log.Warn("daily report: reading receiver error events failed", "err", err)
		} else {
			report.errors = errs
		}
	}

	for _, id := range ids {
		s := byID[id]
		report.receivers = append(report.receivers, receiverReportLine{
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
			report.stale = append(report.stale, staleReceiverLine{id: id, staleAfter: recv.StaleAfter, lastSeen: lastSeen})
		}
	}

	return report
}

// renderDailyReportBody renders report as a plain-text email body.
func renderDailyReportBody(report dailyReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "go-backup-tool daily receiver report\n")
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
		b.WriteString("No errors recorded.\n")
	} else {
		b.WriteString("Errors:\n")

		for _, e := range report.errors {
			fmt.Fprintf(&b, "  [%s] %s %s %q: %s\n", e.At.UTC().Format(time.RFC3339), e.ReceiverID, e.Kind, e.Key, e.Error)
		}
	}

	return b.String()
}

// formatReportBytes formats n bytes as a short human-readable size (e.g.
// "1.2 GB") for the daily report body. Unlike a general-purpose humanize
// package, this only needs to read reasonably in an email, not be exact.
func formatReportBytes(n int64) string {
	return backup.FormatSize(n, 1000, "kMGTPE", false)
}

// reportSMTPTimeout bounds the whole daily report email send (connect,
// authenticate, and deliver), since sendDailyReport runs on its own
// background schedule rather than under a run's -timeout.
const reportSMTPTimeout = 30 * time.Second

// sendDailyReport builds and emails rc's daily receiver report for the
// window ending at, logging (rather than returning) any failure: like the
// stale-receiver webhook, a delivery problem here shouldn't affect anything
// else this process is doing, and there's no caller to report it to — the
// next scheduled report gets another chance.
func sendDailyReport(ctx context.Context, rc *backup.RunConfig, db *sql.DB, at time.Time, log *slog.Logger) {
	report := buildDailyReport(ctx, rc, db, at, log)

	subject := "go-backup-tool daily report - " + at.Format("2006-01-02")
	body := renderDailyReportBody(report)

	sendCtx, cancel := context.WithTimeout(ctx, reportSMTPTimeout)
	defer cancel()

	if err := sendMail(sendCtx, rc.Report.SMTP, rc.Report.From, rc.Report.To, subject, body); err != nil {
		log.Warn("daily report: sending email failed", "err", err)
		return
	}

	log.Info("daily report sent", "to", rc.Report.To, "receivers", len(report.receivers), "errors", len(report.errors), "stale", len(report.stale))
}

// dialSMTP connects to cfg's mail server and returns a ready-to-use
// *smtp.Client: already TLS-wrapped for backup.SMTPSecurityTLS, or with
// STARTTLS already negotiated for backup.SMTPSecurityStartTLS. ctx's
// deadline (see reportSMTPTimeout) is applied directly to the underlying
// connection, since net/smtp's own operations aren't otherwise
// context-aware.
func dialSMTP(ctx context.Context, cfg backup.SMTPSettings) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %q: %w", addr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	var conn net.Conn

	if cfg.Security == backup.SMTPSecurityTLS {
		conn = tls.Client(rawConn, &tls.Config{ServerName: cfg.Host})
	} else {
		conn = rawConn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("initializing smtp client: %w", err)
	}

	if cfg.Security != backup.SMTPSecurityStartTLS {
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
func sendMail(ctx context.Context, cfg backup.SMTPSettings, sender string, recipients []string, subject, body string) error {
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
