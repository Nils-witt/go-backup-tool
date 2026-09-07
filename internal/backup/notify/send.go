package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds a single notification delivery — one webhook POST or one
// email send — since none of this package's callers run under a run's
// -timeout (a receiver's stale/download webhook, or the report loop's own
// background schedule).
const Timeout = 10 * time.Second

// httpClient is shared by every PostWebhook call; Timeout bounds each
// request.
var httpClient = &http.Client{Timeout: Timeout}

// PostWebhook sends body to wh, defaulting its Content-Type header to
// defaultContentType when wh.Headers doesn't set one. The caller owns
// logging/interpreting the result; a non-nil error means the request
// couldn't even be built or sent, not a non-2xx response, which the caller
// checks on the returned *http.Response (and must Close its Body).
func PostWebhook(ctx context.Context, wh Webhook, body []byte, defaultContentType string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, wh.Method, wh.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	for k, v := range wh.Headers {
		req.Header.Set(k, v)
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", defaultContentType)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// SendMail sends an email from sender to every address in recipients, via
// cfg. encrypt, if non-nil (see EmailEncrypt), GPG-encrypts body as an
// OpenPGP/MIME message (RFC 3156) before sending; subject is always sent in
// the clear, since SMTP/MIME headers aren't covered by that encryption.
func SendMail(ctx context.Context, cfg SMTPSettings, sender string, recipients []string, subject, body string, encrypt *EmailEncrypt) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	message, err := buildMailMessage(ctx, sender, recipients, subject, body, encrypt)
	if err != nil {
		return err
	}

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

	if _, err := w.Write([]byte(message)); err != nil {
		_ = w.Close()
		return fmt.Errorf("writing message: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("closing message: %w", err)
	}

	return client.Quit()
}

// buildMailMessage renders the full RFC 5322 message SendMail hands to the
// DATA command: plain text via renderMailMessage, or — when encrypt is set —
// body GPG-encrypted (see encryptGPGBody) and wrapped as OpenPGP/MIME via
// renderEncryptedMailMessage.
func buildMailMessage(ctx context.Context, sender string, recipients []string, subject, body string, encrypt *EmailEncrypt) (string, error) {
	if encrypt == nil {
		return renderMailMessage(sender, recipients, subject, body), nil
	}

	encrypted, err := encryptGPGBody(ctx, *encrypt, body)
	if err != nil {
		return "", fmt.Errorf("gpg-encrypting body: %w", err)
	}

	return renderEncryptedMailMessage(sender, recipients, subject, encrypted), nil
}

// encryptGPGBody GPG-encrypts body (armored) to every one of enc.Recipients,
// which must already have their public key in the keyring enc.GPGBin/
// enc.GPGHomedir resolve to. Mirrors pipeline.buildGPGCommand's recipient
// mode, but runs synchronously since an email body is small enough to hold
// entirely in memory.
func encryptGPGBody(ctx context.Context, enc EmailEncrypt, body string) (string, error) {
	args := []string{"--batch", "--yes"}

	if enc.GPGHomedir != "" {
		args = append(args, "--homedir", enc.GPGHomedir)
	}

	args = append(args, "--trust-model", "always", "--armor", "--encrypt")

	for _, r := range enc.Recipients {
		args = append(args, "--recipient", r)
	}

	cmd := exec.CommandContext(ctx, enc.GPGBin, args...) //nolint:gosec // enc.GPGBin/args are operator-supplied CLI config, not untrusted input
	cmd.Stdin = strings.NewReader(body)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running %s: %w (stderr: %s)", enc.GPGBin, err, strings.TrimSpace(stderr.String()))
	}

	return string(out), nil
}

// dialSMTP connects to cfg's mail server and returns a ready-to-use
// *smtp.Client: already TLS-wrapped for SMTPSecurityTLS, or with STARTTLS
// already negotiated for SMTPSecurityStartTLS. ctx's deadline is applied
// directly to the underlying connection, since net/smtp's own operations
// aren't otherwise context-aware.
func dialSMTP(ctx context.Context, cfg SMTPSettings) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %q: %w", addr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	var conn net.Conn

	if cfg.Security == SMTPSecurityTLS {
		conn = tls.Client(rawConn, &tls.Config{ServerName: cfg.Host})
	} else {
		conn = rawConn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("initializing smtp client: %w", err)
	}

	if cfg.Security != SMTPSecurityStartTLS {
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

// renderMailMessage builds an RFC 5322 message (headers plus body) for
// SendMail's DATA command. Header values are config-file-controlled, not
// network input, but CRLF is still stripped defensively so a stray newline
// in a notification's from/to/subject can never inject an extra header or
// start of body.
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
// an email header (see renderMailMessage/renderEncryptedMailMessage).
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// pgpMIMEBoundary separates renderEncryptedMailMessage's two MIME parts. A
// fixed value is fine: the armored PGP block it wraps is base64-like text,
// which will never itself contain a line starting with "--" followed by this
// exact string.
const pgpMIMEBoundary = "gpg-backup-tool-pgp-mime-boundary"

// renderEncryptedMailMessage builds an RFC 5322 message whose body is
// encryptedBody (an ASCII-armored PGP message from encryptGPGBody) wrapped
// as OpenPGP/MIME (RFC 3156): a multipart/encrypted message a compliant mail
// client (Thunderbird, Apple Mail with GPGMail, Outlook with Gpg4win, ...)
// decrypts automatically. Headers are built the same way renderMailMessage
// builds them; only the body/Content-Type differ.
func renderEncryptedMailMessage(from string, to []string, subject, encryptedBody string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "From: %s\r\n", stripCRLF(from))
	fmt.Fprintf(&b, "To: %s\r\n", stripCRLF(strings.Join(to, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", stripCRLF(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/encrypted; protocol=\"application/pgp-encrypted\"; boundary=\"%s\"\r\n", pgpMIMEBoundary)
	b.WriteString("\r\n")
	b.WriteString("This is an OpenPGP/MIME encrypted message (RFC 3156).\r\n")

	fmt.Fprintf(&b, "--%s\r\n", pgpMIMEBoundary)
	b.WriteString("Content-Type: application/pgp-encrypted\r\n")
	b.WriteString("Content-Description: PGP/MIME version identification\r\n")
	b.WriteString("\r\n")
	b.WriteString("Version: 1\r\n")

	fmt.Fprintf(&b, "--%s\r\n", pgpMIMEBoundary)
	b.WriteString("Content-Type: application/octet-stream; name=\"encrypted.asc\"\r\n")
	b.WriteString("Content-Description: OpenPGP encrypted message\r\n")
	b.WriteString("Content-Disposition: inline; filename=\"encrypted.asc\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.TrimRight(encryptedBody, "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")

	fmt.Fprintf(&b, "--%s--\r\n", pgpMIMEBoundary)

	return b.String()
}
