package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// RetryInterval is how long a failed email waits on a Queue before its next
// delivery attempt.
const RetryInterval = time.Minute

// queuedEmail is one email SendMailQueued couldn't deliver, held on a Queue
// until Run's next tick retries it.
type queuedEmail struct {
	cfg        SMTPSettings
	sender     string
	recipients []string
	subject    string
	body       string
	encrypt    *EmailEncrypt
}

// Queue is an SMTP failure queue: an email SendMailQueued couldn't deliver
// is held here instead of being dropped, and retried every RetryInterval
// (via Run) until it succeeds. The zero value is not usable; construct one
// with NewQueue. Safe for concurrent use.
type Queue struct {
	mu    sync.Mutex
	items []queuedEmail
}

// NewQueue returns an empty Queue, ready for SendMailQueued to enqueue into
// and Run to drain.
func NewQueue() *Queue {
	return &Queue{}
}

// enqueue adds e to q, to be retried the next time Run's ticker fires.
func (q *Queue) enqueue(e queuedEmail) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.items = append(q.items, e)
}

// Len reports how many emails are currently queued for retry.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// Run retries every email on q once every RetryInterval, until ctx is done.
// It's meant to run for the lifetime of the process in its own goroutine
// (mirrors pipeline.RunReportLoop/receiver.MonitorStaleReceivers), started
// once by app.Run.
func (q *Queue) Run(ctx context.Context, log *slog.Logger) {
	log = log.With("component", "smtp-queue")

	ticker := time.NewTicker(RetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.retryAll(ctx, log)
		}
	}
}

// retryAll attempts to resend every email currently on q, putting back only
// the ones that still fail (ahead of anything enqueued while this retry
// round was running).
func (q *Queue) retryAll(ctx context.Context, log *slog.Logger) {
	q.mu.Lock()
	pending := q.items
	q.items = nil
	q.mu.Unlock()

	var stillFailing []queuedEmail

	for _, e := range pending {
		if err := SendMail(ctx, e.cfg, e.sender, e.recipients, e.subject, e.body, e.encrypt); err != nil {
			log.Warn("smtp queue: retry failed", "to", e.recipients, "err", err)
			stillFailing = append(stillFailing, e)

			continue
		}

		log.Info("smtp queue: retry succeeded", "to", e.recipients)
	}

	if len(stillFailing) == 0 {
		return
	}

	q.mu.Lock()
	q.items = append(stillFailing, q.items...)
	q.mu.Unlock()
}

// SendMailQueued sends an email exactly like SendMail; on failure, it also
// queues it on q (if non-nil) so Queue.Run retries it every RetryInterval
// instead of it being lost. The original error is still returned so the
// caller can log/report the immediate failure the same way it always has.
func SendMailQueued(ctx context.Context, q *Queue, cfg SMTPSettings, sender string, recipients []string, subject, body string, encrypt *EmailEncrypt) error {
	err := SendMail(ctx, cfg, sender, recipients, subject, body, encrypt)
	if err == nil {
		return nil
	}

	if q != nil {
		q.enqueue(queuedEmail{cfg: cfg, sender: sender, recipients: recipients, subject: subject, body: body, encrypt: encrypt})
	}

	return err
}
