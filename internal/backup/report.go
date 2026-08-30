package backup

import (
	"errors"
	"fmt"
	"os"
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

// SMTPSecurity is fileSMTP.Security after validation.
type SMTPSecurity string

// The SMTPSecurity values a report.smtp.security: value can resolve to.
const (
	SMTPSecurityStartTLS SMTPSecurity = "starttls"
	SMTPSecurityTLS      SMTPSecurity = "tls"
	SMTPSecurityNone     SMTPSecurity = "none"
)

// SMTPSettings is fileSMTP after validation, with Password already resolved
// from the environment.
type SMTPSettings struct {
	Host     string
	Port     int
	Username string
	Password string
	Security SMTPSecurity
}

// ReportSettings is fileReport after validation, ready for
// pipeline.RunDailyReportLoop to act on. Its zero value (Enabled false)
// means the daily report is disabled.
type ReportSettings struct {
	Enabled bool
	To      []string
	From    string

	// SendHour/SendMinute are fileReport.Time, parsed once here rather than
	// re-parsed on every scheduling loop iteration (see
	// pipeline.nextDailyReportTime).
	SendHour   int
	SendMinute int

	SMTP SMTPSettings
}

// defaultReportTime is fileReport.Time's default when left unset.
const defaultReportTime = "07:00"

// resolveReportSettings validates cfg (the config file's report: entry) and
// resolves it into a ReportSettings, reading report.smtp.password-env from
// the environment if authentication is configured. An unset/false
// cfg.Enabled returns the zero value, leaving the daily report disabled.
func resolveReportSettings(cfg fileReport) (ReportSettings, error) {
	if !cfg.Enabled {
		return ReportSettings{}, nil
	}

	if len(cfg.To) == 0 {
		return ReportSettings{}, errors.New("report.enabled is true but report.to is not set")
	}

	to := make([]string, len(cfg.To))

	for i, addr := range cfg.To {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return ReportSettings{}, fmt.Errorf("report.to[%d] is empty", i)
		}

		to[i] = addr
	}

	sendTime := strings.TrimSpace(cfg.Time)
	if sendTime == "" {
		sendTime = defaultReportTime
	}

	hm, err := time.Parse("15:04", sendTime)
	if err != nil {
		return ReportSettings{}, fmt.Errorf("parsing report.time %q: %w (want 24h \"HH:MM\")", sendTime, err)
	}

	smtpCfg, err := resolveSMTPSettings(&cfg.SMTP)
	if err != nil {
		return ReportSettings{}, err
	}

	from := strings.TrimSpace(cfg.From)
	if from == "" {
		from = smtpCfg.Username
	}

	if from == "" {
		return ReportSettings{}, errors.New("report.enabled is true but neither report.from nor report.smtp.username is set")
	}

	return ReportSettings{
		Enabled:    true,
		To:         to,
		From:       from,
		SendHour:   hm.Hour(),
		SendMinute: hm.Minute(),
		SMTP:       smtpCfg,
	}, nil
}

// resolveSMTPSettings validates cfg (the config file's report.smtp: entry)
// and resolves it into an SMTPSettings, reading password-env from the
// environment when username is set.
func resolveSMTPSettings(cfg *fileSMTP) (SMTPSettings, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return SMTPSettings{}, errors.New("report.enabled is true but report.smtp.host is not set")
	}

	security, err := parseSMTPSecurity(cfg.Security)
	if err != nil {
		return SMTPSettings{}, err
	}

	port := cfg.Port
	if port == 0 {
		port = defaultSMTPPort(security)
	}

	username := strings.TrimSpace(cfg.Username)
	passwordEnv := strings.TrimSpace(cfg.PasswordEnv)

	if err := pairedFieldsErr("report.smtp.username", username, "report.smtp.password-env", passwordEnv); err != nil {
		return SMTPSettings{}, err
	}

	var password string

	if username != "" {
		password = os.Getenv(passwordEnv)
		if password == "" {
			return SMTPSettings{}, fmt.Errorf("report.smtp.password-env: environment variable %q is not set", passwordEnv)
		}
	}

	return SMTPSettings{Host: host, Port: port, Username: username, Password: password, Security: security}, nil
}

// parseSMTPSecurity validates a report.smtp.security: value, defaulting an
// unset value to SMTPSecurityStartTLS.
func parseSMTPSecurity(s string) (SMTPSecurity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(SMTPSecurityStartTLS):
		return SMTPSecurityStartTLS, nil
	case string(SMTPSecurityTLS):
		return SMTPSecurityTLS, nil
	case string(SMTPSecurityNone):
		return SMTPSecurityNone, nil
	default:
		return "", fmt.Errorf("report.smtp.security: unknown value %q (want \"starttls\", \"tls\", or \"none\")", s)
	}
}

// defaultSMTPPort returns security's conventional port, used when
// report.smtp.port is left unset.
func defaultSMTPPort(security SMTPSecurity) int {
	if security == SMTPSecurityTLS {
		return 465
	}

	return 587
}
