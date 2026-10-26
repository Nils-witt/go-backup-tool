package settings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

const (
	// DefaultStatusPushIntervalSeconds is how often this instance pushes
	// its status when StatusPush.IntervalSeconds is 0.
	DefaultStatusPushIntervalSeconds = 30
	// MinStatusPushIntervalSeconds is the shortest StatusPush.IntervalSeconds.
	MinStatusPushIntervalSeconds = 10
	// MaxStatusPushIntervalSeconds is the longest StatusPush.IntervalSeconds.
	MaxStatusPushIntervalSeconds = 3600
)

// StatusPush is where this instance pushes its status to (see the
// peerstatus package): Server, a type: remote server, names the central
// instance, which shows this one's jobs and receivers on its dashboard.
// IntervalSeconds is the longest the central instance goes without a push
// (0 means DefaultStatusPushIntervalSeconds); changes are pushed sooner.
type StatusPush struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	Server          string `yaml:"server" json:"server"`
	IntervalSeconds int    `yaml:"interval-seconds" json:"interval_seconds"`
}

// Interval returns how often p pushes at the latest.
func (p StatusPush) Interval() time.Duration {
	if p.IntervalSeconds == 0 {
		return DefaultStatusPushIntervalSeconds * time.Second
	}

	return time.Duration(p.IntervalSeconds) * time.Second
}

// validate reports what's wrong with p, checking an enabled push's server
// with checkServer when it's set.
func (p StatusPush) validate(checkServer func(name string) error) error {
	if p.IntervalSeconds != 0 && (p.IntervalSeconds < MinStatusPushIntervalSeconds || p.IntervalSeconds > MaxStatusPushIntervalSeconds) {
		return fmt.Errorf("interval-seconds must be 0 (the default, %d) or between %d and %d, got %d",
			DefaultStatusPushIntervalSeconds, MinStatusPushIntervalSeconds, MaxStatusPushIntervalSeconds, p.IntervalSeconds)
	}

	if !p.Enabled {
		return nil
	}

	if p.Server == "" {
		return errors.New("server is required to enable the status push")
	}

	if checkServer == nil {
		return nil
	}

	if err := checkServer(p.Server); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	return nil
}

func statusPushFrom(s store.StatusPushSettings) StatusPush {
	return StatusPush{Enabled: s.Enabled, Server: s.Server, IntervalSeconds: s.IntervalSeconds}
}

// UseServers makes UpdateStatusPush check an enabled push's server with
// check, which returns why a server name can't be pushed to (unknown, or
// not type: remote) — see jobs.Manager.Server. Call it before serving the
// web UI or importing settings.
func (m *Manager) UseServers(check func(name string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.checkServer = check
}

// loadStatusPushLocked applies the stored status push settings, if any.
// The server isn't checked here: servers may load later, and the push
// itself reports a server that doesn't resolve. m.mu must be held.
func (m *Manager) loadStatusPushLocked(ctx context.Context) {
	stored, ok, err := m.db.GetStatusPushSettings(ctx)
	if err != nil {
		m.log.Error("reading status push settings from the state db", "err", err)
		return
	}

	if !ok {
		return
	}

	p := statusPushFrom(stored)
	if err := p.validate(nil); err != nil {
		m.log.Error("status push settings are invalid; the status push stays off until fixed in the web UI", "err", err)
		return
	}

	m.statusPush.Store(&p)
}

// StatusPush returns the status push settings currently in effect.
func (m *Manager) StatusPush() StatusPush { return *m.statusPush.Load() }

// StatusPushChanged receives a value after each UpdateStatusPush, so the
// pusher applies the new settings right away instead of at its next tick.
func (m *Manager) StatusPushChanged() <-chan struct{} { return m.statusPushChanged }

// ManagedStatusPush is the status push settings as the web UI shows them.
// UpdatedAt is zero, and UpdatedBy empty, while none were ever saved (the
// push is then off).
type ManagedStatusPush struct {
	StatusPush

	UpdatedAt time.Time
	UpdatedBy string
}

// GetStatusPush returns the stored status push settings, or the zero
// (disabled) StatusPush if none were ever saved.
func (m *Manager) GetStatusPush(ctx context.Context) (ManagedStatusPush, error) {
	if m.db == nil {
		return ManagedStatusPush{}, ErrStoreUnavailable
	}

	stored, ok, err := m.db.GetStatusPushSettings(ctx)
	if err != nil {
		return ManagedStatusPush{}, err
	}

	if !ok {
		return ManagedStatusPush{}, nil
	}

	return ManagedStatusPush{StatusPush: statusPushFrom(stored), UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy}, nil
}

// UpdateStatusPush validates and stores p as the status push settings and
// applies them (see StatusPushChanged). Validation failures wrap
// ErrInvalid.
func (m *Manager) UpdateStatusPush(ctx context.Context, user string, p StatusPush) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := p.validate(m.checkServer); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if err := m.db.SaveStatusPushSettings(ctx, store.StatusPushSettings{
		Enabled:         p.Enabled,
		Server:          p.Server,
		IntervalSeconds: p.IntervalSeconds,
		UpdatedAt:       time.Now(),
		UpdatedBy:       user,
	}); err != nil {
		return err
	}

	m.statusPush.Store(&p)

	select {
	case m.statusPushChanged <- struct{}{}:
	default: // a change is already pending; the pusher reads the latest
	}

	m.log.Info("status push settings updated",
		"enabled", p.Enabled, "server", p.Server, "interval_seconds", p.IntervalSeconds, "by", user)

	return nil
}
