package webui

import (
	"net/http"
	"strings"
	"sync"
)

// LogBufferCapacity is how many of the most recent log lines StartWebUI's
// LogRingBuffer keeps for the dashboard's log viewer (see handleLogs).
const LogBufferCapacity = 500

// LogRingBuffer is a bounded, concurrency-safe in-memory tail of the most
// recent log lines written to it, for the web UI's log viewer (see
// handleLogs). It's an io.Writer meant to sit alongside the process's real
// log output (see runWithContext in app.go, which fans writes out to both),
// treating each Write call as one line — matching how a slog handler calls
// Write exactly once per record. Lines live only in memory: a restart clears
// it, same as the receiver status store.
type LogRingBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
	start int // index of the oldest entry in lines, once lines is full
}

// NewLogRingBuffer returns an empty LogRingBuffer holding at most capacity
// lines.
func NewLogRingBuffer(capacity int) *LogRingBuffer {
	return &LogRingBuffer{cap: capacity, lines: make([]string, 0, capacity)}
}

// Write records p, trimmed of its trailing newline, as the newest line,
// evicting the oldest one once the buffer is at capacity. Always succeeds,
// so a logger writing through this never fails on that account.
func (b *LogRingBuffer) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.lines) < b.cap {
		b.lines = append(b.lines, line)
	} else {
		b.lines[b.start] = line
		b.start = (b.start + 1) % b.cap
	}

	return len(p), nil
}

// snapshot returns the currently buffered lines, oldest first.
func (b *LogRingBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, len(b.lines))
	for i := range out {
		out[i] = b.lines[(b.start+i)%len(b.lines)]
	}

	return out
}

// handleLogs serves buf's currently buffered log lines as JSON, for the
// dashboard's log viewer to poll.
func handleLogs(buf *LogRingBuffer) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, buf.snapshot())
	}
}
