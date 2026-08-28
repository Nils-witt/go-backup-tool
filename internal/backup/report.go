package backup

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"os"
	"sort"
	"strings"
	"time"
)

// fileReport is the top-level report: entry, configuring the optional daily
// email: an overview of how many files each configured receiver received,
// any receiver API errors, and any receiver currently stale, sent once a
// day to an operator's inbox via SMTP. It's independent of the web UI
// dashboard (webui.go) — useful for anyone monitoring receivers by inbox,
// not just those watching the dashboard — but reads the same
// receiver_events history (schedule_state.go) and on-disk receiver state
// (receiver.go) the dashboard itself uses. Unset (the default) disables it.
type fileReport struct {
	Enabled bool     `yaml:"enabled"`
	To      []string `yaml:"to"`
	// From is the envelope/header sender address. Unset falls back to
	// smtp.username; it's an error to leave both unset.
	From string   `yaml:"from"`
	Time string   `yaml:"time"` // "HH:MM", 24h, in this process's local time zone; default "07:00"
	SMTP fileSMTP `yaml:"smtp"`
}

// fileSMTP is the report.smtp: block, describing how to reach the outgoing
// mail server. Like a server's access-key-env/secret-key-env, Password is
// never written directly in this file: PasswordEnv names an environment
// variable to read it from instead.
type fileSMTP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"` // default depends on security: 465 for "tls", 587 otherwise

	// Username/PasswordEnv authenticate to the mail server with SMTP PLAIN
	// auth, required together; leave both unset to send unauthenticated
	// (e.g. a local relay that only accepts connections from this host).
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password-env"`

	// Security selects the connection's encryption: "starttls" (the
	// default) connects in plaintext and upgrades via STARTTLS before
	// authenticating; "tls" connects already encrypted (the traditional
	// "SMTPS" port, usually 465); "none" never encrypts, for a trusted
	// local relay only.
	Security string `yaml:"security"`
}

// smtpSecurity is fileSMTP.Security after validation.
type smtpSecurity string

const (
	smtpSecurityStartTLS smtpSecurity = "starttls"
	smtpSecurityTLS      smtpSecurity = "tls"
	smtpSecurityNone     smtpSecurity = "none"
)

// smtpSettings is fileSMTP after validation, with Password already resolved
// from the environment.
type smtpSettings struct {
	host     string
	port     int
	username string
	password string
	security smtpSecurity
}

// reportSettings is fileReport after validation, ready for
// runDailyReportLoop to act on. Its zero value (enabled false) means the
// daily report is disabled.
type reportSettings struct {
	enabled bool
	to      []string
	from    string

	// sendHour/sendMinute are fileReport.Time, parsed once here rather than
	// re-parsed on every scheduling loop iteration (see
	// nextDailyReportTime).
	sendHour   int
	sendMinute int

	smtp smtpSettings
}

// defaultReportTime is fileReport.Time's default when left unset.
const defaultReportTime = "07:00"

// resolveReportSettings validates cfg (the config file's report: entry) and
// resolves it into a reportSettings, reading report.smtp.password-env from
// the environment if authentication is configured. An unset/false
// cfg.Enabled returns the zero value, leaving the daily report disabled.
func resolveReportSettings(cfg fileReport) (reportSettings, error) {
	if !cfg.Enabled {
		return reportSettings{}, nil
	}

	if len(cfg.To) == 0 {
		return reportSettings{}, errors.New("report.enabled is true but report.to is not set")
	}

	to := make([]string, len(cfg.To))

	for i, addr := range cfg.To {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return reportSettings{}, fmt.Errorf("report.to[%d] is empty", i)
		}

		to[i] = addr
	}

	sendTime := strings.TrimSpace(cfg.Time)
	if sendTime == "" {
		sendTime = defaultReportTime
	}

	hm, err := time.Parse("15:04", sendTime)
	if err != nil {
		return reportSettings{}, fmt.Errorf("parsing report.time %q: %w (want 24h \"HH:MM\")", sendTime, err)
	}

	smtpCfg, err := resolveSMTPSettings(&cfg.SMTP)
	if err != nil {
		return reportSettings{}, err
	}

	from := strings.TrimSpace(cfg.From)
	if from == "" {
		from = smtpCfg.username
	}

	if from == "" {
		return reportSettings{}, errors.New("report.enabled is true but neither report.from nor report.smtp.username is set")
	}

	return reportSettings{
		enabled:    true,
		to:         to,
		from:       from,
		sendHour:   hm.Hour(),
		sendMinute: hm.Minute(),
		smtp:       smtpCfg,
	}, nil
}

// resolveSMTPSettings validates cfg (the config file's report.smtp: entry)
// and resolves it into an smtpSettings, reading password-env from the
// environment when username is set.
func resolveSMTPSettings(cfg *fileSMTP) (smtpSettings, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return smtpSettings{}, errors.New("report.enabled is true but report.smtp.host is not set")
	}

	security, err := parseSMTPSecurity(cfg.Security)
	if err != nil {
		return smtpSettings{}, err
	}

	port := cfg.Port
	if port == 0 {
		port = defaultSMTPPort(security)
	}

	username := strings.TrimSpace(cfg.Username)
	passwordEnv := strings.TrimSpace(cfg.PasswordEnv)

	if (username == "") != (passwordEnv == "") {
		return smtpSettings{}, errors.New("report.smtp.username and report.smtp.password-env must be set together")
	}

	var password string

	if username != "" {
		password = os.Getenv(passwordEnv)
		if password == "" {
			return smtpSettings{}, fmt.Errorf("report.smtp.password-env: environment variable %q is not set", passwordEnv)
		}
	}

	return smtpSettings{host: host, port: port, username: username, password: password, security: security}, nil
}

// parseSMTPSecurity validates a report.smtp.security: value, defaulting an
// unset value to smtpSecurityStartTLS.
func parseSMTPSecurity(s string) (smtpSecurity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(smtpSecurityStartTLS):
		return smtpSecurityStartTLS, nil
	case string(smtpSecurityTLS):
		return smtpSecurityTLS, nil
	case string(smtpSecurityNone):
		return smtpSecurityNone, nil
	default:
		return "", fmt.Errorf("report.smtp.security: unknown value %q (want \"starttls\", \"tls\", or \"none\")", s)
	}
}

// defaultSMTPPort returns security's conventional port, used when
// report.smtp.port is left unset.
func defaultSMTPPort(security smtpSecurity) int {
	if security == smtpSecurityTLS {
		return 465
	}

	return 587
}

// runDailyReportLoop sends rc's daily receiver report once a day at
// rc.report's configured time, in this process's local time zone, until ctx
// is done. A no-op if the daily report isn't enabled. db may be nil (the
// state db couldn't be opened at startup); the report is still sent, just
// without any receiver_events history (see buildDailyReport).
func runDailyReportLoop(ctx context.Context, rc *runConfig, db *sql.DB, log *slog.Logger) {
	if !rc.report.enabled {
		return
	}

	log = log.With("component", "daily-report")

	for {
		next := nextDailyReportTime(rc.report.sendHour, rc.report.sendMinute, time.Now())
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
	errors     []receiverErrorEvent
	stale      []staleReceiverLine
}

// buildDailyReport summarizes rc's configured receivers' activity in the
// 24h window ending at end: files received and errors from receiver_events
// (db, skipped if nil), and current staleness read live from disk (see
// lastReceivedAt), mirroring the dashboard's own annotateReceiverStaleness
// so the two never disagree. A query failure is logged and leaves that
// section empty rather than failing the whole report — a partial report is
// better than none, matching this codebase's usual failure handling.
func buildDailyReport(ctx context.Context, rc *runConfig, db *sql.DB, end time.Time, log *slog.Logger) dailyReport {
	start := end.Add(-24 * time.Hour)
	report := dailyReport{start: start, end: end}

	ids := make([]string, 0, len(rc.receivers))
	for id := range rc.receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	byID := make(map[string]receiverDaySummary, len(ids))

	if db != nil {
		summaries, err := summarizeReceiverEvents(ctx, db, start, end)
		if err != nil {
			log.Warn("daily report: summarizing receiver events failed", "err", err)
		}

		for _, s := range summaries {
			byID[s.ReceiverID] = s
		}

		errs, err := readReceiverErrorEvents(ctx, db, start, end)
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

		recv := rc.receivers[id]
		if recv.staleAfter <= 0 {
			continue
		}

		lastSeen, ok, err := lastReceivedAt(recv)
		if err != nil {
			log.Warn("daily report: checking receiver staleness failed", "id", id, "err", err)
			continue
		}

		if ok && time.Since(lastSeen) > recv.staleAfter {
			report.stale = append(report.stale, staleReceiverLine{id: id, staleAfter: recv.staleAfter, lastSeen: lastSeen})
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
	const unit = 1000

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
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
func sendDailyReport(ctx context.Context, rc *runConfig, db *sql.DB, at time.Time, log *slog.Logger) {
	report := buildDailyReport(ctx, rc, db, at, log)

	subject := "go-backup-tool daily report - " + at.Format("2006-01-02")
	body := renderDailyReportBody(report)

	sendCtx, cancel := context.WithTimeout(ctx, reportSMTPTimeout)
	defer cancel()

	if err := sendMail(sendCtx, rc.report.smtp, rc.report.from, rc.report.to, subject, body); err != nil {
		log.Warn("daily report: sending email failed", "err", err)
		return
	}

	log.Info("daily report sent", "to", rc.report.to, "receivers", len(report.receivers), "errors", len(report.errors), "stale", len(report.stale))
}

// dialSMTP connects to cfg's mail server and returns a ready-to-use
// *smtp.Client: already TLS-wrapped for smtpSecurityTLS, or with STARTTLS
// already negotiated for smtpSecurityStartTLS. ctx's deadline (see
// reportSMTPTimeout) is applied directly to the underlying connection,
// since net/smtp's own operations aren't otherwise context-aware.
func dialSMTP(ctx context.Context, cfg smtpSettings) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.host, cfg.port)

	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %q: %w", addr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	var conn net.Conn

	if cfg.security == smtpSecurityTLS {
		conn = tls.Client(rawConn, &tls.Config{ServerName: cfg.host})
	} else {
		conn = rawConn
	}

	client, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("initializing smtp client: %w", err)
	}

	if cfg.security != smtpSecurityStartTLS {
		return client, nil
	}

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: cfg.host}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("starting tls: %w", err)
		}
	}

	return client, nil
}

// sendMail sends a plain-text email from sender to every address in
// recipients, via cfg.
func sendMail(ctx context.Context, cfg smtpSettings, sender string, recipients []string, subject, body string) error {
	client, err := dialSMTP(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if cfg.username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.username, cfg.password, cfg.host)); err != nil {
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
