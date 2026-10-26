package peerstatus

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// fakeSettings is a SettingsSource whose settings a test sets.
type fakeSettings struct {
	mu      sync.Mutex
	push    settings.StatusPush
	changed chan struct{}
}

func newFakeSettings(p settings.StatusPush) *fakeSettings {
	return &fakeSettings{push: p, changed: make(chan struct{}, 1)}
}

func (f *fakeSettings) StatusPush() settings.StatusPush {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.push
}

func (f *fakeSettings) StatusPushChanged() <-chan struct{} { return f.changed }

func (f *fakeSettings) set(p settings.StatusPush) {
	f.mu.Lock()
	f.push = p
	f.mu.Unlock()

	f.changed <- struct{}{}
}

// push is one push a test pusher sent.
type push struct {
	target  *config.Target
	payload Payload
}

// startTestPusher runs a pusher over src and jobs, with short delays,
// sending every push to the returned channel. servers maps the names it
// resolves.
func startTestPusher(t *testing.T, src SettingsSource, jobs *status.Store, servers map[string]config.ResolvedServer) <-chan push {
	t.Helper()

	pushes := make(chan push, 16)
	p := &pusher{
		deps: PusherDeps{
			Settings: src,
			Server: func(name string) (config.ResolvedServer, bool) {
				s, ok := servers[name]
				return s, ok
			},
			Status:         jobs,
			Receivers:      receiver.NewRegistry(nil),
			ReceiverStatus: status.NewReceiverStore(nil),
			InstanceName:   "branch",
			Log:            discardLogger,
		},
		coalesce: 10 * time.Millisecond,
		minGap:   20 * time.Millisecond,
		post: func(_ context.Context, target *config.Target, body []byte) error {
			var payload Payload
			if err := json.Unmarshal(body, &payload); err != nil {
				return err
			}

			pushes <- push{target: target, payload: payload}

			return nil
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		p.run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return pushes
}

func remoteServer(t *testing.T, name string) config.ResolvedServer {
	t.Helper()

	s, err := config.ResolveServer(config.FileServer{Name: name, Type: "remote", Endpoint: "https://central.example"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	return s
}

func localServer(t *testing.T, name string) config.ResolvedServer {
	t.Helper()

	s, err := config.ResolveServer(config.FileServer{Name: name, Type: "local", Path: t.TempDir()})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	return s
}

func waitPush(t *testing.T, pushes <-chan push) push {
	t.Helper()

	select {
	case p := <-pushes:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no push within 5s")
		return push{}
	}
}

func expectNoPush(t *testing.T, pushes <-chan push, within time.Duration) {
	t.Helper()

	select {
	case p := <-pushes:
		t.Fatalf("unexpected push %+v", p.payload)
	case <-time.After(within):
	}
}

func TestPusherPushesOnStartAndChange(t *testing.T) {
	t.Parallel()

	jobs := status.NewStore([]*config.Config{{Name: "db"}})
	src := newFakeSettings(settings.StatusPush{Enabled: true, Server: "central", IntervalSeconds: 3600})
	pushes := startTestPusher(t, src, jobs, map[string]config.ResolvedServer{"central": remoteServer(t, "central")})

	first := waitPush(t, pushes)
	if first.target.Endpoint != "https://central.example" || first.target.Kind != config.ServerKindRemote {
		t.Errorf("pushed to %+v, want central's endpoint", first.target)
	}

	if got := first.payload; got.Version != Version || got.InstanceName != "branch" || got.IntervalSeconds != 3600 ||
		len(got.Jobs) != 1 || got.Jobs[0].Name != "db" || got.Receivers == nil {
		t.Errorf("first push = %+v, want version %d, branch, 3600s, job db, receivers listed", got, Version)
	}

	// A burst of changes goes out as one push.
	jobs.Starting("db")
	jobs.Finished("db", nil, 1024, nil)

	if got := waitPush(t, pushes).payload.Jobs[0].State; got != status.StateOK {
		t.Errorf("job state after the burst = %q, want ok", got)
	}

	expectNoPush(t, pushes, 100*time.Millisecond)
}

func TestPusherIdlesWhileDisabled(t *testing.T) {
	t.Parallel()

	jobs := status.NewStore([]*config.Config{{Name: "db"}})
	src := newFakeSettings(settings.StatusPush{Server: "central"})
	pushes := startTestPusher(t, src, jobs, map[string]config.ResolvedServer{"central": remoteServer(t, "central")})

	jobs.Starting("db")
	expectNoPush(t, pushes, 100*time.Millisecond)

	src.set(settings.StatusPush{Enabled: true, Server: "central", IntervalSeconds: 3600})

	if got := waitPush(t, pushes).payload.Jobs[0].State; got != status.StateRunning {
		t.Errorf("job state once enabled = %q, want running", got)
	}
}

func TestPusherHeartbeat(t *testing.T) {
	t.Parallel()

	jobs := status.NewStore(nil)
	src := newFakeSettings(settings.StatusPush{Enabled: true, Server: "central", IntervalSeconds: 1})
	pushes := startTestPusher(t, src, jobs, map[string]config.ResolvedServer{"central": remoteServer(t, "central")})

	waitPush(t, pushes)

	start := time.Now()

	waitPush(t, pushes)

	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("heartbeat after %v, want about the 1s interval", elapsed)
	}
}

func TestPusherOnce(t *testing.T) {
	t.Parallel()

	servers := map[string]config.ResolvedServer{"central": remoteServer(t, "central"), "nas": localServer(t, "nas")}
	p := &pusher{
		deps: PusherDeps{
			Server: func(name string) (config.ResolvedServer, bool) {
				s, ok := servers[name]
				return s, ok
			},
			Status: status.NewStore(nil),
			Log:    discardLogger,
		},
		post: func(context.Context, *config.Target, []byte) error { return errors.New("connection refused") },
	}

	tests := []struct {
		name   string
		server string
	}{
		{name: "unknown server", server: "missing"},
		{name: "local server", server: "nas"},
		{name: "post failure", server: "central"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := p.pushOnce(t.Context(), settings.StatusPush{Enabled: true, Server: tt.server}); err == nil {
				t.Error("pushOnce() error = nil, want one")
			}
		})
	}
}

func TestPusherPushTracksFailures(t *testing.T) {
	t.Parallel()

	servers := map[string]config.ResolvedServer{"central": remoteServer(t, "central")}
	p := &pusher{
		deps: PusherDeps{
			Server: func(name string) (config.ResolvedServer, bool) {
				s, ok := servers[name]
				return s, ok
			},
			Status: status.NewStore(nil),
			Log:    discardLogger,
		},
		post: func(context.Context, *config.Target, []byte) error { return errors.New("connection refused") },
	}

	// A repeated failure is remembered, so it's logged as a warning once.
	sp := settings.StatusPush{Enabled: true, Server: "central"}
	p.push(t.Context(), sp)

	if p.failing == "" {
		t.Error("failing not recorded after a failed push")
	}

	p.post = func(context.Context, *config.Target, []byte) error { return nil }
	p.push(t.Context(), sp)

	if p.failing != "" {
		t.Errorf("failing = %q after a successful push, want cleared", p.failing)
	}
}
