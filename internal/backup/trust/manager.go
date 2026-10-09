package trust

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var (
	// ErrInvalid wraps every validation failure Create/Update return, so
	// the web UI can answer 400 with the message.
	ErrInvalid = errors.New("invalid trusted server")
	// ErrStoreUnavailable is returned by every Manager call when the state
	// db couldn't be opened at startup.
	ErrStoreUnavailable = errors.New("state db unavailable: trusted servers can't be changed this run")
	// ErrInUse is returned by Delete for a trusted server a receiver still
	// allows.
	ErrInUse = errors.New("trusted server is still in use")
)

// Input is a trusted server's editable fields, as the web UI submits them.
type Input struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// ManagedServer is one stored trusted server as the web UI lists it: its
// definition as entered, its key's fingerprint, the receivers allowing it,
// and Error when it no longer resolves and so isn't currently active.
type ManagedServer struct {
	store.TrustedServerConfig

	Fingerprint string
	UsedBy      []string
	Error       string
}

// Manager owns every trusted server's lifecycle: stored in the state db's
// trusted_servers table and resolved into the live registry receivers
// verify requests against. It's the only writer of both.
type Manager struct {
	db       *store.Store
	registry *Registry
	log      *slog.Logger

	// mu serializes mutations, so the db and registry never disagree.
	mu sync.Mutex
	// invalid maps each stored id that failed to resolve at Load to why.
	invalid map[string]string
}

// NewManager builds a Manager over db (nil if the state db couldn't be
// opened), resolving trusted servers into registry. Call Load before
// anything resolves a receiver against registry.
func NewManager(db *store.Store, registry *Registry, log *slog.Logger) *Manager {
	return &Manager{db: db, registry: registry, log: log, invalid: make(map[string]string)}
}

// Registry is the live registry this Manager resolves into.
func (m *Manager) Registry() *Registry { return m.registry }

// Load resolves every stored trusted server into the registry. One that no
// longer resolves is logged and left out (see List).
func (m *Manager) Load(ctx context.Context) {
	if m.db == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stored, err := m.db.ListTrustedServers(ctx)
	if err != nil {
		m.log.Error("reading trusted servers from the state db", "err", err)
		return
	}

	for _, ts := range stored {
		s, err := resolve(ts.ID, ts.Name, ts.PublicKey)
		if err != nil {
			m.log.Error("trusted server is invalid and stays inactive until fixed in the web UI", "id", ts.ID, "err", err)
			m.invalid[ts.ID] = err.Error()

			continue
		}

		m.registry.Put(s)
	}
}

// List returns every stored trusted server, in id order.
func (m *Manager) List(ctx context.Context) ([]ManagedServer, error) {
	if m.db == nil {
		return nil, ErrStoreUnavailable
	}

	stored, err := m.db.ListTrustedServers(ctx)
	if err != nil {
		return nil, err
	}

	users, err := m.usersByServer(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ManagedServer, len(stored))
	for i, ts := range stored {
		out[i] = ManagedServer{TrustedServerConfig: ts, UsedBy: users[ts.ID], Error: m.invalid[ts.ID]}
		if s, ok := m.registry.Get(ts.ID); ok {
			out[i].Fingerprint = s.Fingerprint
		}
	}

	return out, nil
}

// Create validates and stores a new trusted server and activates it.
// Validation failures wrap ErrInvalid; a taken id returns
// store.ErrTrustedServerExists.
func (m *Manager) Create(ctx context.Context, user string, in Input) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	s, err := validate(in)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	ts := store.TrustedServerConfig{
		ID: s.ID, Name: s.Name, PublicKey: normalizePEM(in.PublicKey),
		CreatedAt: now, CreatedBy: user, UpdatedAt: now, UpdatedBy: user,
	}

	if err := m.db.CreateTrustedServer(ctx, ts); err != nil {
		return err
	}

	delete(m.invalid, s.ID)
	m.registry.Put(s)
	m.log.Info("trusted server created", "id", s.ID, "name", s.Name, "fingerprint", s.Fingerprint, "by", user)

	return nil
}

// Update validates and stores trusted server in.ID's new name and public
// key and activates it; every receiver allowing it verifies against the new
// key from the next request on. Validation failures wrap ErrInvalid; an
// unknown id returns store.ErrTrustedServerNotFound.
func (m *Manager) Update(ctx context.Context, user string, in Input) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	s, err := validate(in)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ts := store.TrustedServerConfig{ID: s.ID, Name: s.Name, PublicKey: normalizePEM(in.PublicKey), UpdatedAt: time.Now(), UpdatedBy: user}

	if err := m.db.UpdateTrustedServer(ctx, ts); err != nil {
		return err
	}

	delete(m.invalid, s.ID)
	m.registry.Put(s)
	m.log.Info("trusted server updated", "id", s.ID, "name", s.Name, "fingerprint", s.Fingerprint, "by", user)

	return nil
}

// Delete removes trusted server id, refusing (ErrInUse, naming the
// receivers) while a stored receiver still allows it. An unknown id returns
// store.ErrTrustedServerNotFound.
func (m *Manager) Delete(ctx context.Context, user, id string) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	users, err := m.usersByServer(ctx)
	if err != nil {
		return err
	}

	if u := users[id]; len(u) > 0 {
		return fmt.Errorf("%w by %s", ErrInUse, strings.Join(u, ", "))
	}

	if err := m.db.DeleteTrustedServer(ctx, id); err != nil {
		return err
	}

	delete(m.invalid, id)
	m.registry.Delete(id)
	m.log.Info("trusted server deleted", "id", id, "by", user)

	return nil
}

// usersByServer maps each trusted server id to the stored receivers
// allowing it ("receiver <id>").
func (m *Manager) usersByServer(ctx context.Context) (map[string][]string, error) {
	receivers, err := m.db.ListReceiverConfigs(ctx)
	if err != nil {
		return nil, err
	}

	users := make(map[string][]string)

	for _, rc := range receivers {
		for _, id := range rc.AllowedServers {
			if user := "receiver " + rc.ID; !slices.Contains(users[id], user) {
				users[id] = append(users[id], user)
			}
		}
	}

	return users, nil
}

// validate normalizes and resolves in, wrapping any failure in ErrInvalid.
func validate(in Input) (Server, error) {
	s, err := resolve(in.ID, in.Name, in.PublicKey)
	if err != nil {
		return Server{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return s, nil
}

// resolve checks id is a UUID (a sending instance's server.uuid, shown on
// its Identity page), name is set, and publicKey parses, returning the
// resolved server with id in canonical lowercase form.
func resolve(id, name, publicKey string) (Server, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return Server{}, fmt.Errorf("server id must be the sending instance's UUID: %w", err)
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return Server{}, errors.New("name is required")
	}

	key, err := ParsePublicKey(publicKey)
	if err != nil {
		return Server{}, err
	}

	return Server{ID: parsed.String(), Name: name, PublicKey: key, Fingerprint: Fingerprint(key)}, nil
}

// NormalizeID returns id in the canonical form trusted servers are stored
// and looked up under (a lowercase UUID), or id trimmed if it isn't a UUID.
func NormalizeID(id string) string {
	id = strings.TrimSpace(id)
	if parsed, err := uuid.Parse(id); err == nil {
		return parsed.String()
	}

	return id
}

func normalizePEM(raw string) string {
	return strings.TrimSpace(raw) + "\n"
}
