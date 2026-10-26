// Package telemetry reports this process's errors to Sentry when the config
// file's sentry: entry sets a DSN (see config.SentrySettings): Init sets the
// SDK up, Handler turns error log records into Sentry events (and info/warn
// records into breadcrumbs leading up to them), Recover reports a panic
// before letting it crash the process as before, and HTTPMiddleware does
// the same for a web UI request's panic. With Sentry disabled, every one of
// them is a no-op, so callers wire them in unconditionally.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/version"
)

// flushTimeout bounds how long Flush (and Recover) wait for queued events
// to reach Sentry before the process exits.
const flushTimeout = 2 * time.Second

// Init sets up the Sentry SDK from s, tagging every event with this
// build's version and the instance's serverName. It reports whether Sentry
// is now enabled; with s's DSN empty it does nothing and returns false.
// The returned error means s's DSN was rejected by the SDK, in which case
// Sentry stays disabled.
func Init(s config.SentrySettings, serverName string) (bool, error) {
	if !s.Enabled() {
		return false, nil
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:         s.DSN,
		Environment: s.Environment,
		Release:     Release(),
		ServerName:  serverName,
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			scrubRequest(event)
			return event
		},
	})
	if err != nil {
		return false, fmt.Errorf("initializing sentry: %w", err)
	}

	return true, nil
}

// Release names this build in Sentry, the same for the backend's events
// and the dashboard's (see webui's GET /api/client-config).
func Release() string {
	return "go-backup-tool@" + version.Version
}

// scrubRequest strips event's HTTP request (see HTTPMiddleware) down to
// its method and URL path: its query string, headers, cookies and client
// address may carry the dashboard's bearer tokens, download and live
// tickets, or a client's identity.
func scrubRequest(event *sentry.Event) {
	if event.Request == nil {
		return
	}

	event.Request = &sentry.Request{URL: event.Request.URL, Method: event.Request.Method}
}

// Flush waits up to flushTimeout for queued events to be sent. Call it
// before the process exits; it's a no-op when Sentry isn't initialized.
func Flush() {
	sentry.Flush(flushTimeout)
}

// Recover reports a panic in the calling goroutine to Sentry, then panics
// again with the same value, so the process still crashes as it would
// have. It must be deferred directly: defer telemetry.Recover().
func Recover() {
	r := recover()
	if r == nil {
		return
	}

	sentry.CurrentHub().Recover(r)
	sentry.Flush(flushTimeout)

	panic(r)
}

// HTTPMiddleware reports a panic in next's handler to Sentry before
// re-panicking, so net/http still logs it and closes the connection as
// before. Without it, net/http's own per-request recovery would keep such
// a panic from ever reaching Recover.
func HTTPMiddleware(next http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(next)
}

// Handler returns a slog.Handler reporting to the current Sentry hub: see
// handler. Combine it with the regular log output (slog.NewMultiHandler).
func Handler() slog.Handler {
	return &handler{hub: sentry.CurrentHub()}
}

// handler is the slog.Handler behind Handler. A record at slog.LevelError
// or above becomes a Sentry event; one at info or warn only becomes a
// breadcrumb, attached to the next event for context. Debug records are
// dropped. Events are grouped by the log message alone (see newEvent), so
// error text that varies per run doesn't split one problem into many
// issues.
type handler struct {
	hub    *sentry.Hub
	attrs  []slog.Attr
	groups []string
}

var _ slog.Handler = (*handler)(nil)

// Enabled reports whether level is at least slog.LevelInfo.
func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

// Handle sends r to Sentry as an event or a breadcrumb (see handler).
func (h *handler) Handle(_ context.Context, r slog.Record) error {
	data, errText := h.recordData(r)

	if r.Level < slog.LevelError {
		level := sentry.LevelInfo
		if r.Level >= slog.LevelWarn {
			level = sentry.LevelWarning
		}

		h.hub.AddBreadcrumb(&sentry.Breadcrumb{
			Type:      "default",
			Category:  "log",
			Level:     level,
			Message:   r.Message,
			Data:      data,
			Timestamp: r.Time,
		}, nil)

		return nil
	}

	h.hub.CaptureEvent(newEvent(r, data, errText))

	return nil
}

// WithAttrs returns a handler adding attrs to every record.
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	clone := *h
	clone.attrs = append(slices.Clone(h.attrs), h.qualified(attrs)...)

	return &clone
}

// WithGroup returns a handler qualifying later attributes with name.
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	clone := *h
	clone.groups = slices.Concat(h.groups, []string{name})

	return &clone
}

// qualified returns attrs with h's open groups folded into each key, as
// "group.key".
func (h *handler) qualified(attrs []slog.Attr) []slog.Attr {
	if len(h.groups) == 0 {
		return attrs
	}

	prefix := strings.Join(h.groups, ".") + "."

	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, slog.Attr{Key: prefix + a.Key, Value: a.Value})
	}

	return out
}

// recordData flattens h's attributes and r's own into one map, nested
// groups as dotted keys and every value resolved. errText is the message of
// r's first error-valued attribute (normally "err"), "" if it has none.
func (h *handler) recordData(r slog.Record) (data map[string]any, errText string) {
	data = make(map[string]any, len(h.attrs)+r.NumAttrs())

	for _, a := range h.attrs {
		addAttr(data, "", a)
	}

	recordAttrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		recordAttrs = append(recordAttrs, a)

		if err, ok := a.Value.Resolve().Any().(error); ok && errText == "" {
			errText = err.Error()
		}

		return true
	})

	for _, a := range h.qualified(recordAttrs) {
		addAttr(data, "", a)
	}

	return data, errText
}

// addAttr stores a in data under prefix+a.Key, recursing into groups. An
// error value is stored as its message, since an arbitrary error type
// needn't marshal to JSON.
func addAttr(data map[string]any, prefix string, a slog.Attr) {
	v := a.Value.Resolve()

	switch {
	case a.Equal(slog.Attr{}):
		return
	case v.Kind() == slog.KindGroup:
		groupPrefix := prefix
		if a.Key != "" {
			groupPrefix += a.Key + "."
		}

		for _, ga := range v.Group() {
			addAttr(data, groupPrefix, ga)
		}

		return
	}

	if err, ok := v.Any().(error); ok {
		data[prefix+a.Key] = err.Error()
		return
	}

	data[prefix+a.Key] = v.Any()
}

// newEvent builds the Sentry event for error record r: its message (plus
// errText, when there is one, so the issue reads like the log line), r's
// attributes (data) as the "log" context, and a stack trace of the logging
// goroutine.
func newEvent(r slog.Record, data map[string]any, errText string) *sentry.Event {
	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Logger = "slog"
	event.Timestamp = r.Time
	event.Message = r.Message
	event.Fingerprint = []string{"slog", r.Message}
	event.Contexts["log"] = data
	event.Threads = []sentry.Thread{{Stacktrace: sentry.NewStacktrace(), Current: true}}

	if errText != "" {
		event.Message += ": " + errText
	}

	return event
}
