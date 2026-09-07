package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
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

// SendMail sends a plain-text email from sender to every address in
// recipients, via cfg.
func SendMail(ctx context.Context, cfg SMTPSettings, sender string, recipients []string, subject, body string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

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
// an email header (see renderMailMessage).
func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
