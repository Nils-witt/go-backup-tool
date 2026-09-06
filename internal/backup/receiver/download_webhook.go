package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// DownloadWebhookEvent describes one successful file download, passed to
// NotifyDownloadWebhook for building the request it sends.
type DownloadWebhookEvent struct {
	Username string
	Key      string
	At       time.Time
}

// downloadWebhookPayload is the default JSON body POSTed to a receiver's
// download-webhook:, used unless the receiver's download-webhook.body:
// overrides it (see renderDownloadWebhookPayload).
type downloadWebhookPayload struct {
	User     string    `json:"user"`
	File     string    `json:"file"`
	Receiver string    `json:"receiver"`
	Time     time.Time `json:"time"`
}

// defaultDownloadWebhookContentType is the Content-Type sent with a download
// webhook request whose receiver doesn't set a Content-Type among
// download-webhook.headers:.
const defaultDownloadWebhookContentType = "application/json"

// downloadWebhookBody builds the request body sent to recv.DownloadWebhook:
// recv's download-webhook.body: template (see renderDownloadWebhookPayload),
// if set, otherwise the default JSON downloadWebhookPayload.
func downloadWebhookBody(recv config.ResolvedReceiver, ev DownloadWebhookEvent) ([]byte, error) {
	if recv.DownloadWebhook.Body != "" {
		return []byte(renderDownloadWebhookPayload(recv.DownloadWebhook.Body, recv, ev)), nil
	}

	payload := downloadWebhookPayload{User: ev.Username, File: ev.Key, Receiver: recv.ID, Time: ev.At}

	return json.Marshal(payload)
}

// renderDownloadWebhookPayload substitutes a receiver's
// download-webhook.body: template's placeholders with ev's details: {user},
// {file}, and {receiver} are ev's username/key and recv's id; {time} is
// ev.At formatted as RFC 3339. This lets an operator's webhook receiver
// (Slack, PagerDuty, a custom endpoint expecting its own JSON/form shape,
// ...) get a body it already understands instead of go-backup-tool's own
// default shape.
func renderDownloadWebhookPayload(tmpl string, recv config.ResolvedReceiver, ev DownloadWebhookEvent) string {
	replacer := strings.NewReplacer(
		"{user}", ev.Username,
		"{file}", ev.Key,
		"{receiver}", recv.ID,
		"{time}", ev.At.UTC().Format(time.RFC3339),
	)

	return replacer.Replace(tmpl)
}

// NotifyDownloadWebhook sends ev to recv.DownloadWebhook, if recv has one
// configured (a no-op otherwise). Logs, rather than returns, any failure: a
// webhook delivery problem shouldn't affect anything else this process is
// doing, and there's no caller waiting on the result — handleDownloadFile
// calls this from its own goroutine so a slow or unreachable webhook URL
// never delays the file already being streamed to the browser.
func NotifyDownloadWebhook(recv config.ResolvedReceiver, ev DownloadWebhookEvent, log *slog.Logger) {
	if recv.DownloadWebhook.URL == "" {
		return
	}

	body, err := downloadWebhookBody(recv, ev)
	if err != nil {
		log.Warn("download webhook: encoding payload failed", "id", recv.ID, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, recv.DownloadWebhook.Method, recv.DownloadWebhook.URL, bytes.NewReader(body))
	if err != nil {
		log.Warn("download webhook: building request failed", "id", recv.ID, "err", err)
		return
	}

	for k, v := range recv.DownloadWebhook.Headers {
		req.Header.Set(k, v)
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", defaultDownloadWebhookContentType)
	}

	resp, err := webhookHTTPClient.Do(req)
	if err != nil {
		log.Warn("download webhook: request failed", "id", recv.ID, "webhook", recv.DownloadWebhook.URL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("download webhook: non-2xx response", "id", recv.ID, "webhook", recv.DownloadWebhook.URL, "status", resp.StatusCode)
		return
	}

	log.Info("download webhook fired", "id", recv.ID, "webhook", recv.DownloadWebhook.URL, "user", ev.Username, "file", ev.Key)
}
