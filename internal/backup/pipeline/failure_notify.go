package pipeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// jobFailurePayload is the default JSON body POSTed to a job-failure
// notification's webhook:, used unless its body: overrides it (see
// renderJobFailurePayload).
type jobFailurePayload struct {
	Job        string    `json:"job"`
	Error      string    `json:"error"`
	State      string    `json:"state"`
	Duration   string    `json:"duration"`
	StartedAt  time.Time `json:"started_at"`
	ServerName string    `json:"server_name,omitempty"`
}

// defaultJobFailureWebhookContentType is the Content-Type sent with a
// job-failure notification's webhook request when it doesn't set a
// Content-Type among webhook.headers:.
const defaultJobFailureWebhookContentType = "application/json"

// defaultJobFailureSubject is a job-failure notification's email.subject
// default when left unset.
const defaultJobFailureSubject = "[{server_name}] job {job} {state}"

// defaultJobFailureBody is a job-failure notification's email.body default
// when left unset.
const defaultJobFailureBody = "Job {job} {state} after {duration} (started {started_at}): {error}"

// jobFailureWebhookBody builds the request body sent to wh: wh.Body: (see
// renderJobFailurePayload), if set, otherwise the default JSON
// jobFailurePayload.
func jobFailureWebhookBody(wh notify.Webhook, job *config.Config, jobErr error, state backup.RunState, start time.Time, duration time.Duration) ([]byte, error) {
	if wh.Body != "" {
		return []byte(renderJobFailurePayload(wh.Body, job, jobErr, state, start, duration)), nil
	}

	payload := jobFailurePayload{
		Job: job.Name, Error: jobErr.Error(), State: string(state),
		Duration: duration.String(), StartedAt: start, ServerName: job.ServerName,
	}

	return json.Marshal(payload)
}

// renderJobFailurePayload substitutes a job-failure notification's
// webhook.body:/email.subject:/email.body: template's placeholders: {job}
// and {error} are job's name and jobErr's text; {state} is state ("failed"
// or "incomplete"); {duration} is the run's elapsed time; {started_at} is
// start formatted as RFC 3339; {server_name} is the config file's top-level
// server-name: (see config.Config.ServerName), "" if unset. This lets an
// operator's webhook receiver (Slack, PagerDuty, a custom endpoint expecting
// its own JSON/form shape, ...) or email get a body it already understands
// instead of go-backup-tool's own default shape.
func renderJobFailurePayload(tmpl string, job *config.Config, jobErr error, state backup.RunState, start time.Time, duration time.Duration) string {
	replacer := strings.NewReplacer(
		"{job}", job.Name,
		"{error}", jobErr.Error(),
		"{state}", string(state),
		"{duration}", duration.String(),
		"{started_at}", start.UTC().Format(time.RFC3339),
		"{server_name}", job.ServerName,
	)

	return replacer.Replace(tmpl)
}

// notifyJobFailure sends job's just-finished failure to every one of
// job.FailureNotifications, looked up in notifications (a no-op if there
// are none; an unknown id is logged and skipped). Logs, rather than
// returns, any delivery failure: a notification delivery problem shouldn't
// affect the run that already finished, and there's no caller waiting on
// the result — mirrors receiver.notifyStaleReceiver/NotifyDownload's
// fire-and-forget style. queue, if non-nil, retries a failed email later
// instead of losing it (see notify.SendMailQueued).
func notifyJobFailure(job *config.Config, notifications *notify.Registry, jobErr error, state backup.RunState, start time.Time, duration time.Duration, queue *notify.Queue, log *slog.Logger) {
	for _, n := range notifications.Resolve(job.FailureNotifications, log, "job", job.Name) {
		if n.Webhook != nil {
			notifyJobFailureWebhook(job, *n.Webhook, jobErr, state, start, duration, log)
		}

		if n.Email != nil {
			notifyJobFailureEmail(job, *n.Email, jobErr, state, start, duration, queue, log)
		}
	}
}

// notifyJobFailureWebhook POSTs job's just-finished failure to wh.
func notifyJobFailureWebhook(job *config.Config, wh notify.Webhook, jobErr error, state backup.RunState, start time.Time, duration time.Duration, log *slog.Logger) {
	body, err := jobFailureWebhookBody(wh, job, jobErr, state, start, duration)
	if err != nil {
		log.Warn("job failure webhook: encoding payload failed", "job", job.Name, "err", err)
		return
	}

	resp, err := notify.PostWebhook(context.Background(), wh, body, defaultJobFailureWebhookContentType)
	if err != nil {
		log.Warn("job failure webhook: request failed", "job", job.Name, "webhook", wh.URL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("job failure webhook: non-2xx response", "job", job.Name, "webhook", wh.URL, "status", resp.StatusCode)
		return
	}

	log.Info("job failure webhook fired", "job", job.Name, "webhook", wh.URL, "state", state)
}

// notifyJobFailureEmail emails job's just-finished failure via email. queue,
// if non-nil, retries delivery later on failure instead of losing it.
func notifyJobFailureEmail(job *config.Config, email notify.Email, jobErr error, state backup.RunState, start time.Time, duration time.Duration, queue *notify.Queue, log *slog.Logger) {
	subjectTmpl := email.Subject
	if subjectTmpl == "" {
		subjectTmpl = defaultJobFailureSubject
	}

	bodyTmpl := email.Body
	if bodyTmpl == "" {
		bodyTmpl = defaultJobFailureBody
	}

	subject := renderJobFailurePayload(subjectTmpl, job, jobErr, state, start, duration)
	body := renderJobFailurePayload(bodyTmpl, job, jobErr, state, start, duration)

	ctx, cancel := context.WithTimeout(context.Background(), notify.Timeout)
	defer cancel()

	if err := notify.SendMailQueued(ctx, queue, email.SMTP, email.From, email.To, subject, body, email.Encrypt); err != nil {
		log.Warn("job failure email: sending failed, queued for retry", "job", job.Name, "to", email.To, "err", err)
		return
	}

	log.Info("job failure email sent", "job", job.Name, "to", email.To, "state", state)
}
