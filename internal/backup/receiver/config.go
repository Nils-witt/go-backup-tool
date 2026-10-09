package receiver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var (
	// ErrInvalidReceiver wraps every validation failure Create/Update
	// return, so the web UI can answer 400 with the message.
	ErrInvalidReceiver = errors.New("invalid receiver")
	// ErrReceiverStoreUnavailable is returned by every Manager mutation
	// when the state db couldn't be opened at startup.
	ErrReceiverStoreUnavailable = errors.New("state db unavailable: receivers can't be changed this run")
)

// ManagedReceiver is one stored receiver as the web UI lists it: its
// definition as entered, plus Error when it no longer resolves (e.g. it
// names a notification since removed from the config file) and so isn't
// currently active.
type ManagedReceiver struct {
	store.ReceiverConfig

	Error string
}

// Manager owns every receiver's lifecycle: stored in the state db's
// receivers table, resolved into the live registry, and mirrored into the
// dashboard's status store. It's the only writer of all three, so a
// receiver created, edited, or deleted in the web UI takes effect
// everywhere immediately, without a restart.
type Manager struct {
	db            *store.Store
	registry      *backup.ReceiverRegistry
	status        *backup.ReceiverStatusStore
	notifications *notify.Registry
	serverName    string
	baseDir       string
	log           *slog.Logger

	// mu serializes mutations, so the db, registry, and status store never
	// disagree on an id's state.
	mu sync.Mutex
	// invalid maps each stored receiver id that failed to resolve at Load
	// to why, for List.
	invalid map[string]string
}

// NewManager builds a Manager over db (nil if the state db couldn't be
// opened), resolving receivers against the live notification registry and
// serverName (see config.ResolveReceiver) and restricting web UI paths to baseDir (see
// config.ValidateReceiverPath). Call Load before serving anything.
func NewManager(db *store.Store, registry *backup.ReceiverRegistry, status *backup.ReceiverStatusStore, notifications *notify.Registry, serverName, baseDir string, log *slog.Logger) *Manager {
	return &Manager{
		db: db, registry: registry, status: status,
		notifications: notifications, serverName: serverName, baseDir: baseDir,
		log: log, invalid: make(map[string]string),
	}
}

// BaseDir is the directory every web UI receiver path must lie inside, ""
// if unset.
func (m *Manager) BaseDir() string { return m.baseDir }

// NotificationIDs returns every notification id a receiver may reference
// in stale-notifications:/download-notifications:, sorted.
func (m *Manager) NotificationIDs() []string {
	return m.notifications.IDs()
}

// configFileUser is recorded as created_by/updated_by on receivers imported
// from the config file's receivers:.
const configFileUser = "config file"

// Load imports yamlReceivers (the config file's deprecated receivers:
// entries) into the state db — only ids not stored yet, so edits made in the
// web UI since are kept — then resolves every stored receiver into the
// registry and status store. A stored receiver that no longer resolves is
// logged and left out (see List). With no state db, yamlReceivers are
// resolved in memory instead.
func (m *Manager) Load(ctx context.Context, yamlReceivers []config.FileReceiver) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.db == nil {
		for _, fr := range yamlReceivers {
			m.activateLocked(ctx, fr)
		}

		return
	}

	if len(yamlReceivers) > 0 {
		now := time.Now()
		rows := make([]store.ReceiverConfig, len(yamlReceivers))

		for i, fr := range yamlReceivers {
			rows[i] = toStoreConfig(fr)
			rows[i].CreatedAt, rows[i].CreatedBy = now, configFileUser
			rows[i].UpdatedAt, rows[i].UpdatedBy = now, configFileUser
		}

		imported, err := m.db.ImportReceiverConfigs(ctx, rows)
		if err != nil {
			m.log.Error("importing config file receivers into the state db", "err", err)
		}

		m.log.Warn("receivers: in the config file is deprecated: receivers are managed in the web UI now; remove the receivers: section from the config file",
			"imported", imported, "already_stored", len(yamlReceivers)-len(imported))
	}

	stored, err := m.db.ListReceiverConfigs(ctx)
	if err != nil {
		m.log.Error("reading receivers from the state db", "err", err)
		return
	}

	for _, rc := range stored {
		m.activateLocked(ctx, toFileReceiver(rc))
	}
}

// activateLocked resolves fr and puts it in the registry and status store,
// or records why it doesn't resolve. m.mu must be held.
func (m *Manager) activateLocked(ctx context.Context, fr config.FileReceiver) {
	recv, err := config.ResolveReceiver(fr, m.notifications, m.serverName)
	if err != nil {
		m.log.Error("receiver is invalid and stays inactive until fixed in the web UI", "id", fr.ID, "err", err)
		m.invalid[fr.ID] = err.Error()

		return
	}

	delete(m.invalid, recv.ID)
	m.registry.Put(recv)
	m.status.Upsert(recv)

	if m.db != nil {
		seedReceiverStatus(ctx, m.db, recv.ID, m.status, m.log)
	}
}

// List returns every stored receiver, in id order.
func (m *Manager) List(ctx context.Context) ([]ManagedReceiver, error) {
	if m.db == nil {
		return nil, ErrReceiverStoreUnavailable
	}

	stored, err := m.db.ListReceiverConfigs(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ManagedReceiver, len(stored))
	for i, rc := range stored {
		out[i] = ManagedReceiver{ReceiverConfig: rc, Error: m.invalid[rc.ID]}
	}

	return out, nil
}

// Create validates and stores a new receiver and activates it. Validation
// failures wrap ErrInvalidReceiver; a taken id returns
// store.ErrReceiverExists.
func (m *Manager) Create(ctx context.Context, user string, fr config.FileReceiver) error {
	if m.db == nil {
		return ErrReceiverStoreUnavailable
	}

	fr = normalizeFileReceiver(fr)

	recv, err := m.validate(fr, "")
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	rc := toStoreConfig(fr)
	rc.CreatedAt, rc.CreatedBy = now, user
	rc.UpdatedAt, rc.UpdatedBy = now, user

	if err := m.db.CreateReceiverConfig(ctx, rc); err != nil {
		return err
	}

	delete(m.invalid, recv.ID)
	m.registry.Put(recv)
	m.status.Upsert(recv)
	seedReceiverStatus(ctx, m.db, recv.ID, m.status, m.log)

	m.log.Info("receiver created", "id", recv.ID, "path", recv.Path, "by", user)

	return nil
}

// Update validates and stores receiver fr.ID's new definition and activates
// it. The path must lie inside the base dir unless it's unchanged, so a
// receiver imported from the config file with a path elsewhere can still
// have its other fields edited. Validation failures wrap ErrInvalidReceiver;
// an unknown id returns store.ErrReceiverNotFound.
func (m *Manager) Update(ctx context.Context, user string, fr config.FileReceiver) error {
	if m.db == nil {
		return ErrReceiverStoreUnavailable
	}

	fr = normalizeFileReceiver(fr)

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok, err := m.db.GetReceiverConfig(ctx, fr.ID)
	if err != nil {
		return err
	}

	if !ok {
		return store.ErrReceiverNotFound
	}

	recv, err := m.validate(fr, existing.Path)
	if err != nil {
		return err
	}

	rc := toStoreConfig(fr)
	rc.UpdatedAt, rc.UpdatedBy = time.Now(), user

	if err := m.db.UpdateReceiverConfig(ctx, rc); err != nil {
		return err
	}

	delete(m.invalid, recv.ID)
	m.registry.Put(recv)
	m.status.Upsert(recv)

	m.log.Info("receiver updated", "id", recv.ID, "path", recv.Path, "by", user)

	return nil
}

// Delete removes receiver id from the state db, registry, and status store.
// Its files on disk and its history are kept. An unknown id returns
// store.ErrReceiverNotFound.
func (m *Manager) Delete(ctx context.Context, user, id string) error {
	if m.db == nil {
		return ErrReceiverStoreUnavailable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.db.DeleteReceiverConfig(ctx, id); err != nil {
		return err
	}

	delete(m.invalid, id)
	m.registry.Delete(id)
	m.status.Remove(id)

	m.log.Info("receiver deleted", "id", id, "by", user)

	return nil
}

// validate resolves fr and checks its path against the base dir, skipping
// the path check when it equals unchangedPath (the stored path on Update,
// "" on Create).
func (m *Manager) validate(fr config.FileReceiver, unchangedPath string) (config.ResolvedReceiver, error) {
	recv, err := config.ResolveReceiver(fr, m.notifications, m.serverName)
	if err != nil {
		return config.ResolvedReceiver{}, fmt.Errorf("%w: %w", ErrInvalidReceiver, err)
	}

	if unchangedPath == "" || fr.Path != unchangedPath {
		if err := config.ValidateReceiverPath(m.baseDir, fr.Path); err != nil {
			return config.ResolvedReceiver{}, fmt.Errorf("%w: %w", ErrInvalidReceiver, err)
		}
	}

	return recv, nil
}

// normalizeFileReceiver trims fr's single-line fields and cleans its path,
// so the stored value is what's actually used.
func normalizeFileReceiver(fr config.FileReceiver) config.FileReceiver {
	fr.ID = strings.TrimSpace(fr.ID)
	fr.Path = strings.TrimSpace(fr.Path)

	if fr.Path != "" {
		fr.Path = filepath.Clean(fr.Path)
	}

	fr.Retention = strings.TrimSpace(fr.Retention)
	fr.StaleAfter = strings.TrimSpace(fr.StaleAfter)
	fr.PublicKey = strings.TrimSpace(fr.PublicKey) + "\n"

	return fr
}

func toStoreConfig(fr config.FileReceiver) store.ReceiverConfig {
	return store.ReceiverConfig{
		ID: strings.TrimSpace(fr.ID), PublicKey: fr.PublicKey, Path: fr.Path,
		Retention: fr.Retention, StaleAfter: fr.StaleAfter,
		StaleNotifications: fr.StaleNotifications, DownloadNotifications: fr.DownloadNotifications,
	}
}

func toFileReceiver(rc store.ReceiverConfig) config.FileReceiver {
	return config.FileReceiver{
		ID: rc.ID, PublicKey: rc.PublicKey, Path: rc.Path,
		Retention: rc.Retention, StaleAfter: rc.StaleAfter,
		StaleNotifications: rc.StaleNotifications, DownloadNotifications: rc.DownloadNotifications,
	}
}
