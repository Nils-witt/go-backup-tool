package notify

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"
)

// discardLogger is a *slog.Logger that writes nowhere, for tests that need
// to pass one but don't assert on its output.
var discardLogger = slog.New(slog.DiscardHandler)

func TestRetryIntervalIsOneMinute(t *testing.T) {
	t.Parallel()

	if RetryInterval != time.Minute {
		t.Errorf("RetryInterval = %v, want 1m", RetryInterval)
	}
}

// unreachableSMTPSettings returns SMTPSettings pointed at an address nothing
// is listening on, so SendMail against it reliably fails without needing a
// real unreachable host.
func unreachableSMTPSettings(t *testing.T) SMTPSettings {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}

	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort() error: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi() error: %v", err)
	}

	return SMTPSettings{Host: host, Port: port, Security: SMTPSecurityNone}
}

func TestSendMailQueuedSuccessDoesNotEnqueue(t *testing.T) {
	t.Parallel()

	srv := startFakeSMTPServer(t)
	host, port := srv.hostPort(t)
	cfg := SMTPSettings{Host: host, Port: port, Security: SMTPSecurityNone}

	q := NewQueue()

	err := SendMailQueued(context.Background(), q, cfg, "from@example.com", []string{"to@example.com"}, "s", "b", nil)
	if err != nil {
		t.Fatalf("SendMailQueued() error: %v", err)
	}

	if got := q.Len(); got != 0 {
		t.Errorf("q.Len() = %d, want 0 after a successful send", got)
	}
}

func TestSendMailQueuedFailureEnqueues(t *testing.T) {
	t.Parallel()

	cfg := unreachableSMTPSettings(t)
	q := NewQueue()

	err := SendMailQueued(context.Background(), q, cfg, "from@example.com", []string{"to@example.com"}, "s", "b", nil)
	if err == nil {
		t.Fatal("SendMailQueued() error = nil, want a connection error")
	}

	if got := q.Len(); got != 1 {
		t.Errorf("q.Len() = %d, want 1 after a failed send", got)
	}
}

func TestSendMailQueuedNilQueueDoesNotPanic(t *testing.T) {
	t.Parallel()

	cfg := unreachableSMTPSettings(t)

	err := SendMailQueued(context.Background(), nil, cfg, "from@example.com", []string{"to@example.com"}, "s", "b", nil)
	if err == nil {
		t.Fatal("SendMailQueued() error = nil, want a connection error")
	}
}

// TestQueueRetryAllDeliversOnceReachable exercises the retry path
// (Queue.retryAll, driven on a real schedule by Run) directly rather than
// waiting a full RetryInterval: an email first fails against an unreachable
// address, then succeeds once retried against a fake server that's now
// listening, and is removed from the queue once delivered.
func TestQueueRetryAllDeliversOnceReachable(t *testing.T) {
	t.Parallel()

	unreachable := unreachableSMTPSettings(t)
	q := NewQueue()

	if err := SendMailQueued(context.Background(), q, unreachable, "from@example.com", []string{"to@example.com"}, "s", "b", nil); err == nil {
		t.Fatal("SendMailQueued() error = nil, want a connection error")
	}

	if got := q.Len(); got != 1 {
		t.Fatalf("q.Len() = %d, want 1 after the initial failure", got)
	}

	// Point the queued item's config at a now-reachable fake server, as if
	// the outage had cleared, then drive one retry round directly.
	srv := startFakeSMTPServer(t)
	host, port := srv.hostPort(t)
	q.items[0].cfg = SMTPSettings{Host: host, Port: port, Security: SMTPSecurityNone}

	q.retryAll(context.Background(), discardLogger)

	if got := q.Len(); got != 0 {
		t.Errorf("q.Len() = %d, want 0 once the retry succeeds", got)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.messages) != 1 {
		t.Fatalf("server received %d messages, want 1", len(srv.messages))
	}
}

// TestQueueRetryAllKeepsStillFailingItems checks that an item still failing
// after a retry round stays on the queue instead of being dropped.
func TestQueueRetryAllKeepsStillFailingItems(t *testing.T) {
	t.Parallel()

	cfg := unreachableSMTPSettings(t)
	q := NewQueue()

	if err := SendMailQueued(context.Background(), q, cfg, "from@example.com", []string{"to@example.com"}, "s", "b", nil); err == nil {
		t.Fatal("SendMailQueued() error = nil, want a connection error")
	}

	q.retryAll(context.Background(), discardLogger)

	if got := q.Len(); got != 1 {
		t.Errorf("q.Len() = %d, want 1 to still be queued after another failed retry", got)
	}
}

// TestQueueRunStopsOnContextDone checks that Run returns once ctx is done
// instead of blocking forever, without needing to wait out a real
// RetryInterval tick.
func TestQueueRunStopsOnContextDone(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})

	q := NewQueue()

	go func() {
		q.Run(ctx, discardLogger)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after ctx was canceled")
	}
}
