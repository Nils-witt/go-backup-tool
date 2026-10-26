package peerstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

const (
	// pushCoalesceDelay is how long the pusher waits after a status change
	// before pushing, so a run's burst of updates (start, each target,
	// finish) goes out as one push.
	pushCoalesceDelay = 2 * time.Second

	// pushMinGap is the least time between two pushes a status change can
	// trigger, so a busy instance doesn't push several times a second.
	pushMinGap = 5 * time.Second

	// pushTimeout bounds one push, so an unresponsive central instance
	// doesn't hold back the next one.
	pushTimeout = 30 * time.Second
)

// SettingsSource is where the pusher reads its settings from:
// settings.Manager.
type SettingsSource interface {
	StatusPush() settings.StatusPush
	StatusPushChanged() <-chan struct{}
}

// PusherDeps is what RunPusher reads.
type PusherDeps struct {
	Settings SettingsSource

	// Server resolves the server the settings name (see
	// jobs.Manager.Server) at every push, so a server edited in the web UI
	// applies at once.
	Server func(name string) (config.ResolvedServer, bool)

	Identity       *identity.ServerIdentity
	Status         *status.Store
	Receivers      *receiver.Registry
	ReceiverStatus *status.ReceiverStore

	// InstanceName is this instance's server-name: (see
	// config.RunConfig.ServerName).
	InstanceName string

	Log *slog.Logger
}

// pusher is RunPusher's state.
type pusher struct {
	deps PusherDeps

	// post sends one push; remoteclient.PostStatus, replaced in tests.
	post func(ctx context.Context, t *config.Target, body []byte) error

	// coalesce and minGap are pushCoalesceDelay and pushMinGap, shortened
	// in tests.
	coalesce, minGap time.Duration

	// failing is the last push's error text, "" while pushes succeed; a
	// failure is logged as a warning only when it differs, so an
	// unreachable central instance doesn't log one every interval.
	failing string
}

// RunPusher pushes this instance's status to the server its settings name,
// until ctx is done: soon after every job or receiver status change, and at
// the latest every settings.StatusPush.Interval as a heartbeat. It idles
// while the push is disabled, and picks up settings changes at once. A
// failed push is logged and retried at the next change or interval.
func RunPusher(ctx context.Context, deps PusherDeps) {
	p := &pusher{
		deps:     deps,
		coalesce: pushCoalesceDelay,
		minGap:   pushMinGap,
		post: func(ctx context.Context, t *config.Target, body []byte) error {
			return remoteclient.PostStatus(ctx, deps.Identity, t, body)
		},
	}

	p.run(ctx)
}

// run is RunPusher's loop.
func (p *pusher) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	next := time.Now()

	var last time.Time

	for {
		sp := p.deps.Settings.StatusPush()

		var due <-chan time.Time

		if sp.Enabled {
			timer.Reset(time.Until(next))
			due = timer.C
		}

		select {
		case <-ctx.Done():
			return
		case <-p.deps.Settings.StatusPushChanged():
			next = time.Now()
		case <-p.deps.Status.Changed():
			next = p.soonest(next, last)
		case <-p.receiversChanged():
			next = p.soonest(next, last)
		case <-due:
			p.push(ctx, sp)

			last = time.Now()
			next = last.Add(sp.Interval())
		}
	}
}

// receiversChanged is the receiver status store's Changed channel, or nil
// (never ready) without one.
func (p *pusher) receiversChanged() <-chan struct{} {
	if p.deps.ReceiverStatus == nil {
		return nil
	}

	return p.deps.ReceiverStatus.Changed()
}

// soonest returns when to push after a status change, given the push
// already scheduled for next and the last one at last: after the coalescing
// delay, but no sooner than minGap after the last push, and never later
// than next.
func (p *pusher) soonest(next, last time.Time) time.Time {
	at := time.Now().Add(p.coalesce)
	if earliest := last.Add(p.minGap); at.Before(earliest) {
		at = earliest
	}

	if next.Before(at) {
		return next
	}

	return at
}

// push sends one push to sp's server, logging a failure.
func (p *pusher) push(ctx context.Context, sp settings.StatusPush) {
	err := p.pushOnce(ctx, sp)
	if err == nil {
		if p.failing != "" {
			p.deps.Log.Info("status push: pushing again", "server", sp.Server)
		}

		p.failing = ""

		return
	}

	level := slog.LevelWarn
	if err.Error() == p.failing {
		level = slog.LevelDebug
	}

	p.failing = err.Error()
	p.deps.Log.Log(ctx, level, "status push failed", "server", sp.Server, "err", err)
}

// pushOnce builds and sends one push to sp's server.
func (p *pusher) pushOnce(ctx context.Context, sp settings.StatusPush) error {
	srv, err := resolveServer(p.deps.Server, sp.Server)
	if err != nil {
		return err
	}

	body, err := json.Marshal(p.payload(sp))
	if err != nil {
		return fmt.Errorf("encoding status: %w", err)
	}

	if len(body) > maxBody {
		return errors.New("status is too large to push")
	}

	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	target := srv.Target("")

	return p.post(ctx, &target, body)
}

// CheckServer reports why server (see PusherDeps.Server) can't resolve
// name to a server a status push can go to; see settings.Manager.UseServers.
func CheckServer(server func(name string) (config.ResolvedServer, bool), name string) error {
	_, err := resolveServer(server, name)
	return err
}

// resolveServer looks name up with server, which must find a type: remote
// server.
func resolveServer(server func(name string) (config.ResolvedServer, bool), name string) (config.ResolvedServer, error) {
	srv, ok := server(name)
	if !ok {
		return config.ResolvedServer{}, fmt.Errorf("no active server named %q", name)
	}

	if srv.Kind() != config.ServerKindRemote {
		return config.ResolvedServer{}, fmt.Errorf("server %q is type %s; a status push needs a type: remote server", name, srv.Kind())
	}

	return srv, nil
}

// payload returns this instance's current status as a push.
func (p *pusher) payload(sp settings.StatusPush) Payload {
	out := Payload{
		Version:         Version,
		InstanceName:    p.deps.InstanceName,
		IntervalSeconds: int(sp.Interval() / time.Second),
		SentAt:          time.Now(),
		Jobs:            p.deps.Status.Snapshot(),
		Receivers:       []status.ReceiverSnapshot{},
	}

	if p.deps.ReceiverStatus != nil {
		out.Receivers = receiver.Snapshots(p.deps.Receivers, p.deps.ReceiverStatus, p.deps.Log)
	}

	return out
}
