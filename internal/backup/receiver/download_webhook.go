package receiver

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// DownloadWebhookEvent describes one successful file download, passed to
// NotifyDownload for building the notification it sends.
type DownloadWebhookEvent struct {
	Username string
	Key      string
	At       time.Time
}

// downloadWebhookPayload is the default JSON body POSTed to a download
// notification's webhook:, used unless its body: overrides it (see
// renderDownloadWebhookPayload).
type downloadWebhookPayload struct {
	User       string    `json:"user"`
	File       string    `json:"file"`
	Receiver   string    `json:"receiver"`
	Time       time.Time `json:"time"`
	ServerName string    `json:"server_name,omitempty"`
}

// defaultDownloadWebhookContentType is the Content-Type sent with a
// download notification's webhook request when it doesn't set a
// Content-Type among webhook.headers:.
const defaultDownloadWebhookContentType = "application/json"

// defaultDownloadSubject is a download notification's email.subject default
// when left unset.
const defaultDownloadSubject = "[{server_name}] {user} downloaded {file}"

// defaultDownloadBody is a download notification's email.body default when
// left unset.
const defaultDownloadBody = "{user} downloaded {file} from receiver {receiver} at {time}."

// downloadWebhookBody builds the request body sent to wh: wh.Body: (see
// renderDownloadWebhookPayload), if set, otherwise the default JSON
// downloadWebhookPayload.
func downloadWebhookBody(wh notify.Webhook, recv config.ResolvedReceiver, ev DownloadWebhookEvent) ([]byte, error) {
	if wh.Body != "" {
		return []byte(renderDownloadWebhookPayload(wh.Body, recv, ev)), nil
	}

	payload := downloadWebhookPayload{User: ev.Username, File: ev.Key, Receiver: recv.ID, Time: ev.At, ServerName: recv.ServerName}

	return json.Marshal(payload)
}

// renderDownloadWebhookPayload substitutes a download notification's
// webhook.body:/email.subject:/email.body: template's placeholders with
// ev's details: {user}, {file}, and {receiver} are ev's username/key and
// recv's id; {time} is ev.At formatted as RFC 3339; {server_name} is the
// config file's top-level server-name: (see
// config.ResolvedReceiver.ServerName), "" if unset. This lets an operator's
// webhook receiver (Slack, PagerDuty, a custom endpoint expecting its own
// JSON/form shape, ...) or email get a body it already understands instead
// of go-backup-tool's own default shape.
func renderDownloadWebhookPayload(tmpl string, recv config.ResolvedReceiver, ev DownloadWebhookEvent) string {
	replacer := strings.NewReplacer(
		"{user}", ev.Username,
		"{file}", ev.Key,
		"{receiver}", recv.ID,
		"{time}", ev.At.UTC().Format(time.RFC3339),
		"{server_name}", recv.ServerName,
	)

	return replacer.Replace(tmpl)
}

// NotifyDownload sends ev to every one of recv.DownloadNotifications (a
// no-op if there are none). Logs, rather than returns, any failure: a
// notification delivery problem shouldn't affect anything else this process
// is doing, and there's no caller waiting on the result — handleDownloadFile
// calls this from its own goroutine so a slow or unreachable destination
// never delays the file already being streamed to the browser. queue, if
// non-nil, retries a failed email later instead of losing it.
func NotifyDownload(recv config.ResolvedReceiver, ev DownloadWebhookEvent, queue *notify.Queue, log *slog.Logger) {
	for _, n := range recv.DownloadNotifications {
		if n.Webhook != nil {
			notifyDownloadWebhook(recv, *n.Webhook, ev, log)
		}

		if n.Email != nil {
			notifyDownloadEmail(recv, *n.Email, ev, queue, log)
		}
	}
}

// notifyDownloadWebhook POSTs ev to wh.
func notifyDownloadWebhook(recv config.ResolvedReceiver, wh notify.Webhook, ev DownloadWebhookEvent, log *slog.Logger) {
	body, err := downloadWebhookBody(wh, recv, ev)
	if err != nil {
		log.Warn("download webhook: encoding payload failed", "id", recv.ID, "err", err)
		return
	}

	resp, err := notify.PostWebhook(context.Background(), wh, body, defaultDownloadWebhookContentType)
	if err != nil {
		log.Warn("download webhook: request failed", "id", recv.ID, "webhook", wh.URL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("download webhook: non-2xx response", "id", recv.ID, "webhook", wh.URL, "status", resp.StatusCode)
		return
	}

	log.Info("download webhook fired", "id", recv.ID, "webhook", wh.URL, "user", ev.Username, "file", ev.Key)
}

// notifyDownloadEmail emails ev via email. queue, if non-nil, retries
// delivery later on failure instead of losing it.
func notifyDownloadEmail(recv config.ResolvedReceiver, email notify.Email, ev DownloadWebhookEvent, queue *notify.Queue, log *slog.Logger) {
	subjectTmpl := email.Subject
	if subjectTmpl == "" {
		subjectTmpl = defaultDownloadSubject
	}

	bodyTmpl := email.Body
	if bodyTmpl == "" {
		bodyTmpl = defaultDownloadBody
	}

	subject := renderDownloadWebhookPayload(subjectTmpl, recv, ev)
	body := renderDownloadWebhookPayload(bodyTmpl, recv, ev)

	ctx, cancel := context.WithTimeout(context.Background(), notify.Timeout)
	defer cancel()

	if err := notify.SendMailQueued(ctx, queue, email.SMTP, email.From, email.To, subject, body, email.Encrypt); err != nil {
		log.Warn("download email: sending failed, queued for retry", "id", recv.ID, "to", email.To, "err", err)
		return
	}

	log.Info("download email sent", "id", recv.ID, "to", email.To, "user", ev.Username, "file", ev.Key)
}
