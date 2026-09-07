// Package notify resolves and validates the config file's top-level smtp:
// and notifications: entries, and sends the webhook/email a notification
// resolves to. It's a leaf package (no imports of config/report) so both
// config (receivers) and report can depend on it without an import cycle.
package notify

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"strings"
)

// FileWebhook is a notification's webhook: block: the HTTP request sent when
// that notification fires. Only url is required; method defaults to POST,
// headers is optional (e.g. Content-Type), and body, if unset, defaults to
// the firing trigger's own JSON summary — set it to send a body your own
// webhook receiver (PagerDuty, Slack, ...) already understands, using that
// trigger's {placeholder} syntax.
type FileWebhook struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

// FileEmail is a notification's email: block. To is required; From defaults
// to smtp.username. Subject/Body, if unset, default to the firing trigger's
// own text — set them to override with the same {placeholder} syntax that
// trigger's webhook.body accepts.
type FileEmail struct {
	To      []string `yaml:"to"`
	From    string   `yaml:"from"`
	Subject string   `yaml:"subject"`
	Body    string   `yaml:"body"`

	// Encrypt, if set, GPG-encrypts this email's body (as OpenPGP/MIME, RFC
	// 3156) before it's sent, so the notification's contents aren't readable
	// in transit or at rest in a mail provider's inbox. The subject line
	// itself is never encrypted (SMTP headers are always plaintext).
	Encrypt *FileEmailEncrypt `yaml:"encrypt"`
}

// FileEmailEncrypt is a notification's email.encrypt: block. Recipients
// names the GPG identities (fingerprint or email address) to encrypt to;
// each must already have its public key in the keyring gpg-bin/gpg-homedir
// resolve to (import it once with `gpg --import`, the same prerequisite as
// a job's own recipients: for backups). GPGBin/GPGHomedir mirror a job's
// gpg-bin/gpg-homedir, defaulting to "gpg" and gpg's own default homedir
// respectively.
type FileEmailEncrypt struct {
	Recipients []string `yaml:"recipients"`
	GPGBin     string   `yaml:"gpg-bin"`
	GPGHomedir string   `yaml:"gpg-homedir"`
}

// FileNotification is one top-level notifications: entry: a named
// destination, referenced by id from report.notifications: and a receiver's
// stale-notifications:/download-notifications:. At least one of Webhook/
// Email must be set; both may be set, firing both channels when this
// notification is referenced.
type FileNotification struct {
	ID      string       `yaml:"id"`
	Webhook *FileWebhook `yaml:"webhook"`
	Email   *FileEmail   `yaml:"email"`
}

// FileSMTP is the top-level smtp: entry, describing how to reach the
// outgoing mail server used by any notification with an email: block. Unset
// (a zero value) is only valid when no notification uses email.
// Password/PasswordEnv are mutually exclusive: Password writes the
// credential directly in this file, PasswordEnv instead names an
// environment variable to read it from (like a server's
// access-key-env/secret-key-env).
type FileSMTP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"` // default depends on security: 465 for "tls", 587 otherwise

	// Username authenticates to the mail server with SMTP PLAIN auth,
	// together with exactly one of Password/PasswordEnv; leave all three
	// unset to send unauthenticated (e.g. a local relay that only accepts
	// connections from this host).
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	PasswordEnv string `yaml:"password-env"`

	// Security selects the connection's encryption: "starttls" (the
	// default) connects in plaintext and upgrades via STARTTLS before
	// authenticating; "tls" connects already encrypted (the traditional
	// "SMTPS" port, usually 465); "none" never encrypts, for a trusted
	// local relay only.
	Security string `yaml:"security"`
}

// SMTPSecurity is FileSMTP.Security after validation.
type SMTPSecurity string

// The SMTPSecurity values a smtp.security: value can resolve to.
const (
	SMTPSecurityStartTLS SMTPSecurity = "starttls"
	SMTPSecurityTLS      SMTPSecurity = "tls"
	SMTPSecurityNone     SMTPSecurity = "none"
)

// SMTPSettings is FileSMTP after validation, with Password already resolved
// from the environment. Its zero value (Host == "") means smtp: was left
// unset in the config file.
type SMTPSettings struct {
	Host     string
	Port     int
	Username string
	Password string
	Security SMTPSecurity
}

// Webhook is a FileWebhook after validation, ready to be sent by
// PostWebhook. A nil *Webhook on a Notification means that notification has
// no webhook channel.
type Webhook struct {
	URL     string
	Method  string            // resolved: defaults to http.MethodPost when unset in the config file
	Headers map[string]string // may be nil; Content-Type falls back to a default if not among these
	Body    string            // "" means the firing trigger's default body
}

// Email is a FileEmail after validation, ready to be sent by SendMail. A nil
// *Email on a Notification means that notification has no email channel.
type Email struct {
	To      []string
	From    string
	Subject string       // "" means the firing trigger's default subject
	Body    string       // "" means the firing trigger's default body
	SMTP    SMTPSettings // the top-level smtp: entry this email sends through

	// Encrypt is a FileEmailEncrypt after validation; nil means this email
	// sends its body in plain text.
	Encrypt *EmailEncrypt
}

// EmailEncrypt is a FileEmailEncrypt after validation, ready to be passed to
// SendMail. GPGBin defaults to "gpg" when unset in the config file.
type EmailEncrypt struct {
	Recipients []string
	GPGBin     string
	GPGHomedir string
}

// defaultGPGBin is used for a notification's email.encrypt.gpg-bin when
// unset, matching the built-in default a job's own gpg-bin: falls back to
// (see appconfig.DefaultGPGBin) — duplicated here rather than imported, since
// notify is a leaf package (see the package doc comment).
const defaultGPGBin = "gpg"

// Notification is one FileNotification after validation, ready to be
// referenced by report.notifications:/a receiver's
// stale-notifications:/download-notifications:. Exactly one of Webhook/
// Email being nil means that channel isn't configured for this
// notification; both are never nil at once (Build rejects that).
type Notification struct {
	ID      string
	Webhook *Webhook
	Email   *Email
}

// ResolveSMTP validates cfg (the config file's top-level smtp: entry) and
// resolves it into an SMTPSettings, reading password-env from the
// environment if set. An empty cfg.Host returns the zero value (smtp: was
// left unset), which is only an error later, in Build, if some notification
// actually references it via an email: block.
func ResolveSMTP(cfg FileSMTP) (SMTPSettings, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return SMTPSettings{}, nil
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
	password := cfg.Password

	password, err = resolveSMTPPassword(username, password, passwordEnv)
	if err != nil {
		return SMTPSettings{}, err
	}

	return SMTPSettings{Host: host, Port: port, Username: username, Password: password, Security: security}, nil
}

// resolveSMTPPassword validates the username/password/password-env
// combination from smtp: and resolves the effective password, reading
// password-env from the environment when it's set.
func resolveSMTPPassword(username, password, passwordEnv string) (string, error) {
	if password != "" && passwordEnv != "" {
		return "", errors.New("smtp.password and smtp.password-env are mutually exclusive; set at most one")
	}

	switch {
	case username == "" && (password != "" || passwordEnv != ""):
		return "", errors.New("smtp.password/password-env is set but smtp.username is not")
	case username != "" && password == "" && passwordEnv == "":
		return "", errors.New("smtp.username is set but neither smtp.password nor smtp.password-env is set")
	}

	if passwordEnv != "" {
		password = os.Getenv(passwordEnv)
		if password == "" {
			return "", fmt.Errorf("smtp.password-env: environment variable %q is not set", passwordEnv)
		}
	}

	return password, nil
}

// parseSMTPSecurity validates a smtp.security: value, defaulting an unset
// value to SMTPSecurityStartTLS.
func parseSMTPSecurity(s string) (SMTPSecurity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(SMTPSecurityStartTLS):
		return SMTPSecurityStartTLS, nil
	case string(SMTPSecurityTLS):
		return SMTPSecurityTLS, nil
	case string(SMTPSecurityNone):
		return SMTPSecurityNone, nil
	default:
		return "", fmt.Errorf("smtp.security: unknown value %q (want \"starttls\", \"tls\", or \"none\")", s)
	}
}

// defaultSMTPPort returns security's conventional port, used when smtp.port
// is left unset.
func defaultSMTPPort(security SMTPSecurity) int {
	if security == SMTPSecurityTLS {
		return 465
	}

	return 587
}

// Build validates fileNotifications (the config file's top-level
// notifications: list) and builds an id -> Notification map, requiring every
// entry to have a unique, non-empty id and at least one of webhook: or
// email:. smtp is the already-resolved top-level smtp: entry (see
// ResolveSMTP), required (non-zero) by any entry with an email: block.
func Build(fileNotifications []FileNotification, smtp SMTPSettings) (map[string]Notification, error) {
	notifications := make(map[string]Notification, len(fileNotifications))

	for i, fn := range fileNotifications {
		id := strings.TrimSpace(fn.ID)
		if id == "" {
			return nil, fmt.Errorf("notifications[%d]: id is required", i)
		}

		if _, exists := notifications[id]; exists {
			return nil, fmt.Errorf("notifications[%d]: duplicate notification id %q", i, id)
		}

		if fn.Webhook == nil && fn.Email == nil {
			return nil, fmt.Errorf("notification %q: at least one of webhook or email must be set", id)
		}

		var webhook *Webhook

		if fn.Webhook != nil {
			resolved, err := resolveWebhook(fn.Webhook)
			if err != nil {
				return nil, fmt.Errorf("notification %q: webhook: %w", id, err)
			}

			webhook = &resolved
		}

		var email *Email

		if fn.Email != nil {
			resolved, err := resolveEmail(fn.Email, smtp)
			if err != nil {
				return nil, fmt.Errorf("notification %q: email: %w", id, err)
			}

			email = &resolved
		}

		notifications[id] = Notification{ID: id, Webhook: webhook, Email: email}
	}

	return notifications, nil
}

// resolveWebhook validates and resolves fw (a notification's webhook:
// block): method defaults to http.MethodPost; headers, if any, are copied
// so the Webhook doesn't alias the config file's own map. url is required.
func resolveWebhook(fw *FileWebhook) (Webhook, error) {
	url := strings.TrimSpace(fw.URL)
	if url == "" {
		return Webhook{}, errors.New("url is required")
	}

	method := strings.ToUpper(strings.TrimSpace(fw.Method))
	if method == "" {
		method = http.MethodPost
	}

	var headers map[string]string
	if len(fw.Headers) > 0 {
		headers = make(map[string]string, len(fw.Headers))
		maps.Copy(headers, fw.Headers)
	}

	return Webhook{URL: url, Method: method, Headers: headers, Body: fw.Body}, nil
}

// resolveEmail validates and resolves fe (a notification's email: block)
// against smtp (the top-level smtp: entry, see ResolveSMTP): to is
// required; from defaults to smtp.Username, erroring if both are empty;
// smtp itself must be configured (non-zero Host).
func resolveEmail(fe *FileEmail, smtp SMTPSettings) (Email, error) {
	if smtp.Host == "" {
		return Email{}, errors.New("smtp: is not configured (required for any notification with an email: block)")
	}

	if len(fe.To) == 0 {
		return Email{}, errors.New("to is required")
	}

	to := make([]string, len(fe.To))

	for i, addr := range fe.To {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return Email{}, fmt.Errorf("to[%d] is empty", i)
		}

		to[i] = addr
	}

	from := strings.TrimSpace(fe.From)
	if from == "" {
		from = smtp.Username
	}

	if from == "" {
		return Email{}, errors.New("neither from nor smtp.username is set")
	}

	encrypt, err := resolveEmailEncrypt(fe.Encrypt)
	if err != nil {
		return Email{}, fmt.Errorf("encrypt: %w", err)
	}

	return Email{To: to, From: from, Subject: fe.Subject, Body: fe.Body, SMTP: smtp, Encrypt: encrypt}, nil
}

// resolveEmailEncrypt validates and resolves fe (a notification's
// email.encrypt: block), returning nil when fe itself is nil (no
// encryption). GPGBin defaults to defaultGPGBin when unset.
func resolveEmailEncrypt(fe *FileEmailEncrypt) (*EmailEncrypt, error) {
	if fe == nil {
		return nil, nil
	}

	if len(fe.Recipients) == 0 {
		return nil, errors.New("recipients is required")
	}

	recipients := make([]string, len(fe.Recipients))

	for i, r := range fe.Recipients {
		r = strings.TrimSpace(r)
		if r == "" {
			return nil, fmt.Errorf("recipients[%d] is empty", i)
		}

		recipients[i] = r
	}

	gpgBin := strings.TrimSpace(fe.GPGBin)
	if gpgBin == "" {
		gpgBin = defaultGPGBin
	}

	return &EmailEncrypt{Recipients: recipients, GPGBin: gpgBin, GPGHomedir: strings.TrimSpace(fe.GPGHomedir)}, nil
}
