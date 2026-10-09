package jobs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// ManagedServer is one stored server as the web UI lists it: its definition
// as entered, the jobs using it, and Error when it doesn't resolve and so
// isn't active.
type ManagedServer struct {
	store.ServerConfig

	UsedBy []string
	Error  string
}

// ListServers returns every stored server, in name order.
func (m *Manager) ListServers(ctx context.Context) ([]ManagedServer, error) {
	return listReferenced(ctx, m, kindServer, m.storedServers, func(sc store.ServerConfig) string { return sc.Name },
		func(sc store.ServerConfig, usedBy []string, errText string) ManagedServer {
			return ManagedServer{ServerConfig: sc, UsedBy: usedBy, Error: errText}
		})
}

// ServerOption is an active server a job's targets may name.
type ServerOption struct {
	Name string
	Kind config.ServerKind
}

// Servers returns every active server, in name order: what a job's targets
// may name.
func (m *Manager) Servers() []ServerOption {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]ServerOption, 0, len(m.servers))
	for _, name := range slices.Sorted(maps.Keys(m.servers)) {
		out = append(out, ServerOption{Name: name, Kind: m.servers[name].Kind()})
	}

	return out
}

// CreateServer validates and stores a new server and activates it.
// Validation failures wrap ErrInvalid; a taken name returns
// store.ErrServerExists.
func (m *Manager) CreateServer(ctx context.Context, user string, fs config.FileServer) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	fs = normalizeServer(fs)

	if _, err := config.ResolveServer(fs); err != nil {
		return fmt.Errorf("%w server: %w", ErrInvalid, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	sc := toStoreServer(fs)
	sc.CreatedAt, sc.CreatedBy, sc.UpdatedAt, sc.UpdatedBy = now, user, now, user

	if err := m.db.CreateServerConfig(ctx, sc); err != nil {
		return err
	}

	m.serverDefs[fs.Name] = fs
	m.activateServerLocked(fs.Name)
	// A job left invalid by naming this server before it existed (e.g. one
	// imported with it from the config file) may resolve now.
	m.refreshJobsLocked(ctx)

	m.log.Info("server created", "server", fs.Name, "type", fs.Type, "by", user)

	return nil
}

// UpdateServer validates and stores server fs.Name's new definition and
// applies it to every job using it, from their next run on. It's refused
// (ErrInvalid) if a job using it would no longer resolve, e.g. a target
// overriding retention on a server changed to type: remote. An unknown name
// returns store.ErrServerNotFound.
func (m *Manager) UpdateServer(ctx context.Context, user string, fs config.FileServer) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	fs = normalizeServer(fs)

	resolved, err := config.ResolveServer(fs)
	if err != nil {
		return fmt.Errorf("%w server: %w", ErrInvalid, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	servers := maps.Clone(m.servers)
	servers[fs.Name] = resolved

	if err := m.checkUsersResolveLocked(servers, m.commands, m.usersLocked(true)[fs.Name]); err != nil {
		return err
	}

	sc := toStoreServer(fs)
	sc.UpdatedAt, sc.UpdatedBy = time.Now(), user

	if err := m.db.UpdateServerConfig(ctx, sc); err != nil {
		return err
	}

	m.serverDefs[fs.Name] = fs
	m.activateServerLocked(fs.Name)
	m.refreshJobsLocked(ctx)

	m.log.Info("server updated", "server", fs.Name, "type", fs.Type, "by", user)

	return nil
}

// DeleteServer removes server name, refusing (ErrInUse, naming the jobs)
// while a stored job's target names it. What was written to it is kept. An
// unknown name returns store.ErrServerNotFound.
func (m *Manager) DeleteServer(ctx context.Context, user, name string) error {
	return deleteReferenced(ctx, m, user, kindServer, name, m.db.DeleteServerConfig, m.serverDefs, m.servers)
}

// ManagedCommand is one stored command as the web UI lists it: its
// definition as entered, the jobs using it, and Error when it doesn't
// resolve and so isn't active.
type ManagedCommand struct {
	store.CommandConfig

	UsedBy []string
	Error  string
}

// ListCommands returns every stored command, in id order.
func (m *Manager) ListCommands(ctx context.Context) ([]ManagedCommand, error) {
	return listReferenced(ctx, m, kindCommand, m.storedCommands, func(cc store.CommandConfig) string { return cc.ID },
		func(cc store.CommandConfig, usedBy []string, errText string) ManagedCommand {
			return ManagedCommand{CommandConfig: cc, UsedBy: usedBy, Error: errText}
		})
}

// CommandIDs returns every active command's id, sorted: what a job target's
// on-error/on-recover may name.
func (m *Manager) CommandIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return slices.Sorted(maps.Keys(m.commands))
}

// CreateCommand validates and stores a new command and activates it.
// Validation failures wrap ErrInvalid; a taken id returns
// store.ErrCommandExists.
func (m *Manager) CreateCommand(ctx context.Context, user string, fc config.FileCommand) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	fc = normalizeCommand(fc)

	if _, err := config.ResolveCommand(fc); err != nil {
		return fmt.Errorf("%w command: %w", ErrInvalid, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	cc := toStoreCommand(fc)
	cc.CreatedAt, cc.CreatedBy, cc.UpdatedAt, cc.UpdatedBy = now, user, now, user

	if err := m.db.CreateCommandConfig(ctx, cc); err != nil {
		return err
	}

	m.commandDefs[fc.ID] = fc
	m.activateCommandLocked(fc.ID)
	m.refreshJobsLocked(ctx)

	m.log.Info("command created", "command", fc.ID, "by", user)

	return nil
}

// UpdateCommand validates and stores command fc.ID's new definition; every
// target using it runs the new one from its next firing on. An unknown id
// returns store.ErrCommandNotFound.
func (m *Manager) UpdateCommand(ctx context.Context, user string, fc config.FileCommand) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	fc = normalizeCommand(fc)

	if _, err := config.ResolveCommand(fc); err != nil {
		return fmt.Errorf("%w command: %w", ErrInvalid, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cc := toStoreCommand(fc)
	cc.UpdatedAt, cc.UpdatedBy = time.Now(), user

	if err := m.db.UpdateCommandConfig(ctx, cc); err != nil {
		return err
	}

	m.commandDefs[fc.ID] = fc
	m.activateCommandLocked(fc.ID)
	m.refreshJobsLocked(ctx)

	m.log.Info("command updated", "command", fc.ID, "by", user)

	return nil
}

// DeleteCommand removes command id, refusing (ErrInUse, naming the jobs)
// while a stored job's target runs it on error or recovery. An unknown id
// returns store.ErrCommandNotFound.
func (m *Manager) DeleteCommand(ctx context.Context, user, id string) error {
	return deleteReferenced(ctx, m, user, kindCommand, id, m.db.DeleteCommandConfig, m.commandDefs, m.commands)
}

// listReferenced lists every stored server or command (kind) via list,
// wrapping each with the jobs using it and why it doesn't resolve, if it
// doesn't; key is its name or id.
func listReferenced[S, O any](ctx context.Context, m *Manager, kind string, list func(context.Context) ([]S, error), key func(S) string, wrap func(S, []string, string) O) ([]O, error) {
	stored, err := list(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	users := m.usersLocked(kind == kindServer)

	out := make([]O, len(stored))
	for i, sc := range stored {
		k := key(sc)
		out[i] = wrap(sc, users[k], m.invalid[kind][k])
	}

	return out, nil
}

// deleteReferenced removes stored server or command (kind) key via del,
// refusing (ErrInUse, naming the jobs) while a stored job uses it, and
// forgets it from defs and active.
func deleteReferenced[D, R any](ctx context.Context, m *Manager, user, kind, key string, del func(context.Context, string) error, defs map[string]D, active map[string]R) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if u := m.usersLocked(kind == kindServer)[key]; len(u) > 0 {
		return fmt.Errorf("%s %w by %s", kind, ErrInUse, strings.Join(u, ", "))
	}

	if err := del(ctx, key); err != nil {
		return err
	}

	delete(defs, key)
	delete(active, key)
	delete(m.invalid[kind], key)

	m.log.Info(kind+" deleted", "id", key, "by", user)

	return nil
}

// checkUsersResolveLocked checks that every job in users ("job <name>", see
// usersLocked) that resolves today still resolves against servers and
// commands, wrapping the first failure in ErrInvalid. m.mu must be held.
func (m *Manager) checkUsersResolveLocked(servers map[string]config.ResolvedServer, commands map[string]config.Command, users []string) error {
	for _, user := range users {
		name := strings.TrimPrefix(user, "job ")
		if _, invalid := m.invalid[kindJob][name]; invalid {
			continue
		}

		if _, err := config.ResolveJob(m.jobDefs[name], m.settings.GPG, servers, commands, m.settings.ServerName); err != nil {
			return fmt.Errorf("%w: job %q would no longer be valid: %w", ErrInvalid, name, err)
		}
	}

	return nil
}
