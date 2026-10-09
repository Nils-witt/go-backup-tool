// Package jobs manages the backup jobs this instance runs, and the servers
// and commands they reference, as an admin edits them in the web UI: stored
// in the state db, resolved into the live job set the scheduler and the
// dashboard read, and imported once from the config file's deprecated
// jobs:/servers:/commands: entries.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var (
	// ErrInvalid wraps every validation failure a Manager mutation returns,
	// so the web UI can answer 400 with the message.
	ErrInvalid = errors.New("invalid")
	// ErrStoreUnavailable is returned by every Manager mutation when the
	// state db couldn't be opened at startup.
	ErrStoreUnavailable = errors.New("state db unavailable: jobs can't be changed this run")
	// ErrEditingDisabled is returned by every Manager mutation unless the
	// config file sets webui.job-editing: true: jobs, servers, and commands
	// then come from the config file.
	ErrEditingDisabled = errors.New("jobs, servers, and commands are managed in the config file: edit them there, or set webui.job-editing: true to manage them in the web UI")
	// ErrInUse is returned when deleting a server or command a job still
	// references.
	ErrInUse = errors.New("still in use")
)

// configFileUser is recorded as created_by/updated_by on everything
// imported from the config file.
const configFileUser = "config file"

// Settings are what every job resolves against besides its own definition,
// all from the config file.
type Settings struct {
	// GPG is the top-level gpg-bin/gpg-homedir every job falls back to.
	GPG notify.GPGSettings
	// ServerName is this instance's {server_name} (see
	// config.RunConfig.ServerName).
	ServerName string
	// Editing (webui.job-editing:) makes the state db the source of jobs,
	// servers, and commands, managed in the web UI. Off, the config file is,
	// read afresh at every startup, and the web UI only shows them.
	Editing bool
	// Filter is -job: only the job of that name is activated.
	Filter string
}

// schedule is one job's running scheduler goroutine (see
// pipeline.Runner.ScheduleLive).
type schedule struct {
	cancel    context.CancelFunc
	done      chan struct{}
	interval  time.Duration
	startTime time.Time
	stopped   bool // canceled, though a run in flight may still be finishing
}

// Manager owns every job's, server's, and command's lifecycle: stored in the
// state db, resolved into the live job set, mirrored into the dashboard's
// status store, and scheduled on the runner. It's the only writer of all of
// them, so a change made in the web UI takes effect immediately, without a
// restart: a job picks up an edit (or an edit to a server or command it
// uses) from its next run on, and is only rescheduled when its interval or
// start-time changed.
type Manager struct {
	db            *store.Store
	status        *backup.StatusStore
	runner        *pipeline.Runner
	notifications *notify.Registry
	settings      Settings
	log           *slog.Logger

	// mu guards everything below; mutations hold it throughout, so the db
	// and the live state never disagree.
	mu sync.RWMutex

	// serverDefs/commandDefs/jobDefs are every stored definition, as
	// entered, whether or not it resolves.
	serverDefs  map[string]config.FileServer
	commandDefs map[string]config.FileCommand
	jobDefs     map[string]config.FileJob

	// servers/commands/jobs are the ones that resolve (jobs: and pass the
	// -job filter); invalid maps each stored name that doesn't resolve to
	// why, keyed by kind.
	servers  map[string]config.ResolvedServer
	commands map[string]config.Command
	jobs     map[string]*config.Config
	invalid  map[string]map[string]string

	// runCtx is Start's context, every run's; nil until Start, so jobs
	// activated before then (by Load) aren't scheduled yet.
	runCtx    context.Context //nolint:containedctx // the process-lifetime context runs started later from web UI edits use
	schedules map[string]*schedule
	wg        sync.WaitGroup
}

// The kinds Manager.invalid is keyed by.
const (
	kindServer  = "server"
	kindCommand = "command"
	kindJob     = "job"
)

// NewManager builds a Manager over db (nil if the state db couldn't be
// opened), mirroring jobs into status and running them on runner. Job
// failure-notifications are checked against notifications. Call Load, then
// Start.
func NewManager(db *store.Store, status *backup.StatusStore, runner *pipeline.Runner, notifications *notify.Registry, settings Settings, log *slog.Logger) *Manager {
	return &Manager{
		db: db, status: status, runner: runner, notifications: notifications, settings: settings, log: log,
		serverDefs: make(map[string]config.FileServer), commandDefs: make(map[string]config.FileCommand), jobDefs: make(map[string]config.FileJob),
		servers: make(map[string]config.ResolvedServer), commands: make(map[string]config.Command), jobs: make(map[string]*config.Config),
		invalid:   map[string]map[string]string{kindServer: {}, kindCommand: {}, kindJob: {}},
		schedules: make(map[string]*schedule),
	}
}

// Editing reports whether mutations are allowed (webui.job-editing:).
func (m *Manager) Editing() bool { return m.settings.Editing }

// fromFile reports whether jobs, servers, and commands come from the config
// file rather than the state db: whenever editing is off, or there's no
// state db to keep them in.
func (m *Manager) fromFile() bool { return m.db == nil || !m.settings.Editing }

// NotificationIDs returns every notification id a job may name in
// failure-notifications, sorted.
func (m *Manager) NotificationIDs() []string { return m.notifications.IDs() }

// Load reads every server, command, and job and resolves them. With editing
// on, that's the state db, after importing the config file's
// servers:/commands:/jobs: into it — only names not stored yet, so edits
// made in the web UI since are kept. With editing off (see fromFile), it's
// the config file's entries, kept in memory only. Anything that doesn't
// resolve is logged and left inactive (see the List methods). It returns an
// error only when the -job filter names no job.
func (m *Manager) Load(ctx context.Context, fileServers []config.FileServer, fileCommands []config.FileCommand, fileJobs []config.FileJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fromFile() {
		for _, fs := range fileServers {
			m.serverDefs[strings.TrimSpace(fs.Name)] = normalizeServer(fs)
		}

		for _, fc := range fileCommands {
			m.commandDefs[strings.TrimSpace(fc.ID)] = normalizeCommand(fc)
		}

		for _, fj := range fileJobs {
			m.jobDefs[strings.TrimSpace(fj.Name)] = normalizeJob(fj)
		}

		m.warnIgnoredStoredLocked(ctx)
	} else {
		m.importLocked(ctx, fileServers, fileCommands, fileJobs)
		m.readStoredLocked(ctx)
	}

	for name := range m.serverDefs {
		m.activateServerLocked(name)
	}

	for id := range m.commandDefs {
		m.activateCommandLocked(id)
	}

	for _, name := range slices.Sorted(maps.Keys(m.jobDefs)) {
		m.activateJobLocked(ctx, name, false)

		if err := config.CheckNotificationRefs(m.jobDefs[name].FailureNotifications, m.notifications); err != nil {
			m.log.Error("job failure-notifications references a notification that doesn't exist; it will be skipped", "job", name, "err", err)
		}
	}

	if f := m.settings.Filter; f != "" {
		if _, ok := m.jobDefs[f]; !ok {
			return fmt.Errorf("-job %q: no such job", f)
		}
	}

	return nil
}

// importLocked carries the config file's deprecated sections over into the
// state db. m.mu must be held.
func (m *Manager) importLocked(ctx context.Context, fileServers []config.FileServer, fileCommands []config.FileCommand, fileJobs []config.FileJob) {
	now := time.Now()

	if len(fileServers) > 0 {
		rows := make([]store.ServerConfig, len(fileServers))
		for i, fs := range fileServers {
			rows[i] = toStoreServer(normalizeServer(fs))
			rows[i].CreatedAt, rows[i].CreatedBy, rows[i].UpdatedAt, rows[i].UpdatedBy = now, configFileUser, now, configFileUser
		}

		imported, err := m.db.ImportServerConfigs(ctx, rows)
		m.logImport("servers", len(rows), imported, err)
	}

	if len(fileCommands) > 0 {
		rows := make([]store.CommandConfig, len(fileCommands))
		for i, fc := range fileCommands {
			rows[i] = toStoreCommand(normalizeCommand(fc))
			rows[i].CreatedAt, rows[i].CreatedBy, rows[i].UpdatedAt, rows[i].UpdatedBy = now, configFileUser, now, configFileUser
		}

		imported, err := m.db.ImportCommandConfigs(ctx, rows)
		m.logImport("commands", len(rows), imported, err)
	}

	if len(fileJobs) > 0 {
		rows := make([]store.JobConfig, len(fileJobs))
		for i, fj := range fileJobs {
			rows[i] = toStoreJob(normalizeJob(fj))
			rows[i].CreatedAt, rows[i].CreatedBy, rows[i].UpdatedAt, rows[i].UpdatedBy = now, configFileUser, now, configFileUser
		}

		imported, err := m.db.ImportJobConfigs(ctx, rows)
		m.logImport("jobs", len(rows), imported, err)
	}
}

func (m *Manager) logImport(section string, total int, imported []string, err error) {
	if err != nil {
		m.log.Error("importing config file "+section+" into the state db", "err", err)
	}

	m.log.Warn(section+": in the config file is ignored once imported, since webui.job-editing manages "+section+" in the web UI; remove the "+section+": section from the config file",
		"imported", imported, "already_stored", total-len(imported))
}

// warnIgnoredStoredLocked warns when the state db still holds jobs from an
// earlier run with editing on: with it off, they're ignored in favor of the
// config file. m.mu must be held.
func (m *Manager) warnIgnoredStoredLocked(ctx context.Context) {
	if m.db == nil {
		return
	}

	stored, err := m.db.ListJobConfigs(ctx)
	if err != nil || len(stored) == 0 {
		return
	}

	m.log.Warn("the state db holds jobs managed in the web UI, which are ignored while webui.job-editing is off: jobs come from the config file",
		"stored_jobs", len(stored))
}

// fromConfigFile lists defs (a config file section) in key order as the
// state db would, converted by conv and marked by mark as made by the config
// file. m.mu must be held.
func fromConfigFile[D, C any](defs map[string]D, conv func(D) C, mark func(*C)) []C {
	out := make([]C, 0, len(defs))

	for _, key := range slices.Sorted(maps.Keys(defs)) {
		c := conv(defs[key])
		mark(&c)
		out = append(out, c)
	}

	return out
}

// storedJobs lists every job, from the state db or the config file (see
// fromFile).
func (m *Manager) storedJobs(ctx context.Context) ([]store.JobConfig, error) {
	if !m.fromFile() {
		return m.db.ListJobConfigs(ctx)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return fromConfigFile(m.jobDefs, toStoreJob, func(c *store.JobConfig) { c.CreatedBy, c.UpdatedBy = configFileUser, configFileUser }), nil
}

// storedServers is storedJobs for servers.
func (m *Manager) storedServers(ctx context.Context) ([]store.ServerConfig, error) {
	if !m.fromFile() {
		return m.db.ListServerConfigs(ctx)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return fromConfigFile(m.serverDefs, toStoreServer, func(c *store.ServerConfig) { c.CreatedBy, c.UpdatedBy = configFileUser, configFileUser }), nil
}

// storedCommands is storedJobs for commands.
func (m *Manager) storedCommands(ctx context.Context) ([]store.CommandConfig, error) {
	if !m.fromFile() {
		return m.db.ListCommandConfigs(ctx)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return fromConfigFile(m.commandDefs, toStoreCommand, func(c *store.CommandConfig) { c.CreatedBy, c.UpdatedBy = configFileUser, configFileUser }), nil
}

// NotificationRefs maps every job's name to its failure-notifications ids —
// what settings.Manager checks before a notification may be deleted.
func (m *Manager) NotificationRefs() map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	refs := make(map[string][]string, len(m.jobDefs))
	for name, fj := range m.jobDefs {
		refs[name] = slices.Clone(fj.FailureNotifications)
	}

	return refs
}

// readStoredLocked loads every stored definition into the *Defs maps. m.mu
// must be held.
func (m *Manager) readStoredLocked(ctx context.Context) {
	servers, err := m.db.ListServerConfigs(ctx)
	if err != nil {
		m.log.Error("reading servers from the state db", "err", err)
	}

	for _, sc := range servers {
		m.serverDefs[sc.Name] = fromStoreServer(sc)
	}

	commands, err := m.db.ListCommandConfigs(ctx)
	if err != nil {
		m.log.Error("reading commands from the state db", "err", err)
	}

	for _, cc := range commands {
		m.commandDefs[cc.ID] = fromStoreCommand(cc)
	}

	jobs, err := m.db.ListJobConfigs(ctx)
	if err != nil {
		m.log.Error("reading jobs from the state db", "err", err)
	}

	for _, jc := range jobs {
		m.jobDefs[jc.Name] = FileJobFrom(jc)
	}
}

// Start schedules every active job on m's runner, running them under ctx
// (the process's lifetime): see pipeline.Runner.ScheduleLive. Jobs created
// or rescheduled from then on are scheduled as they change.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.runCtx = ctx

	for _, name := range slices.Sorted(maps.Keys(m.jobs)) {
		pipeline.WarnIfKeyWontChange(m.log, m.jobs[name])
		m.scheduleLocked(m.jobs[name], false)
	}
}

// Wait blocks until every job's schedule has ended: every one-shot job has
// run, and every repeating one has stopped since Start's context is done.
// Don't call it while the web UI may still change jobs.
func (m *Manager) Wait() {
	m.wg.Wait()
}

// Get returns job name's current definition, reporting false if it isn't
// active. It's a pipeline.JobLookup.
func (m *Manager) Get(name string) (*config.Config, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	job, ok := m.jobs[name]

	return job, ok
}

// Jobs returns every active job, in name order.
func (m *Manager) Jobs() []*config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*config.Config, 0, len(m.jobs))
	for _, name := range slices.Sorted(maps.Keys(m.jobs)) {
		out = append(out, m.jobs[name])
	}

	return out
}

// Names returns every active job's name, sorted.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return slices.Sorted(maps.Keys(m.jobs))
}

// activateServerLocked resolves stored server name into m.servers, or
// records why it doesn't resolve. m.mu must be held.
func (m *Manager) activateServerLocked(name string) {
	s, err := config.ResolveServer(m.serverDefs[name])
	if err != nil {
		m.log.Error("server is invalid and stays inactive until fixed in the web UI", "server", name, "err", err)
		m.invalid[kindServer][name] = err.Error()
		delete(m.servers, name)

		return
	}

	delete(m.invalid[kindServer], name)
	m.servers[name] = s
}

// activateCommandLocked resolves stored command id into m.commands, or
// records why it doesn't resolve. m.mu must be held.
func (m *Manager) activateCommandLocked(id string) {
	c, err := config.ResolveCommand(m.commandDefs[id])
	if err != nil {
		m.log.Error("command is invalid and stays inactive until fixed in the web UI", "command", id, "err", err)
		m.invalid[kindCommand][id] = err.Error()
		delete(m.commands, id)

		return
	}

	delete(m.invalid[kindCommand], id)
	m.commands[id] = c
}

// activateJobLocked resolves stored job name and makes it active — in the
// live job set and the status store, and (once started) scheduled — or
// records why it doesn't resolve and deactivates it. An already-active job
// is only rescheduled when its interval or start-time changed, as resumed
// (see pipeline.Runner.ScheduleLive) unless fresh. m.mu must be held.
func (m *Manager) activateJobLocked(ctx context.Context, name string, fresh bool) {
	job, err := config.ResolveJob(m.jobDefs[name], m.settings.GPG, m.servers, m.commands, m.settings.ServerName)
	if err != nil {
		if _, wasValid := m.invalid[kindJob][name]; !wasValid {
			m.log.Error("job is invalid and stays inactive until fixed in the web UI", "job", name, "err", err)
		}

		m.invalid[kindJob][name] = err.Error()
		m.deactivateJobLocked(name)

		return
	}

	delete(m.invalid[kindJob], name)

	if m.settings.Filter != "" && name != m.settings.Filter {
		return
	}

	_, existed := m.jobs[name]
	m.jobs[name] = job
	m.status.Upsert(job)

	if !existed && m.db != nil {
		pipeline.SeedStatusFromState(ctx, m.db, []*config.Config{job}, m.status, m.log)
	}

	if m.runCtx != nil && !m.scheduledLocked(job) {
		m.scheduleLocked(job, existed && !fresh)
	}
}

// scheduledLocked reports whether job's schedule is running unchanged: then
// its scheduler just picks up job's new definition on its next run. m.mu
// must be held.
func (m *Manager) scheduledLocked(job *config.Config) bool {
	s := m.schedules[job.Name]
	if s == nil || s.stopped || s.interval != job.Interval || !s.startTime.Equal(job.StartTime) {
		return false
	}

	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// scheduleLocked (re)starts job's scheduler goroutine, stopping any previous
// one first — which only takes effect once that one's run in flight (if
// any) has finished, so the two never run the job concurrently. m.mu must
// be held.
func (m *Manager) scheduleLocked(job *config.Config, resumed bool) {
	prev := m.schedules[job.Name]
	if prev != nil {
		prev.cancel()
		prev.stopped = true
	}

	stop, cancel := context.WithCancel(m.runCtx)
	s := &schedule{cancel: cancel, done: make(chan struct{}), interval: job.Interval, startTime: job.StartTime}
	m.schedules[job.Name] = s

	m.wg.Go(func() {
		defer close(s.done)
		defer cancel()

		if prev != nil {
			<-prev.done
		}

		m.runner.ScheduleLive(m.runCtx, stop, job, m.Get, resumed)
	})
}

// deactivateJobLocked takes job name out of the live job set and the status
// store and ends its schedule (a run in flight finishes). m.mu must be held.
func (m *Manager) deactivateJobLocked(name string) {
	if s := m.schedules[name]; s != nil {
		s.cancel()
		s.stopped = true
	}

	if _, ok := m.jobs[name]; ok {
		delete(m.jobs, name)
		m.status.Remove(name)
	}
}

// refreshJobsLocked re-resolves every stored job, after a server or command
// they may use changed. m.mu must be held.
func (m *Manager) refreshJobsLocked(ctx context.Context) {
	for _, name := range slices.Sorted(maps.Keys(m.jobDefs)) {
		m.activateJobLocked(ctx, name, false)
	}
}

// checkMutable reports why nothing may be changed right now, if anything.
func (m *Manager) checkMutable() error {
	switch {
	case m.db == nil:
		return ErrStoreUnavailable
	case !m.settings.Editing:
		return ErrEditingDisabled
	default:
		return nil
	}
}

// ManagedJob is one stored job as the web UI lists it: its definition as
// entered, plus Error when it doesn't resolve and so isn't active.
type ManagedJob struct {
	store.JobConfig

	Error string
}

// ListJobs returns every job, in name order.
func (m *Manager) ListJobs(ctx context.Context) ([]ManagedJob, error) {
	stored, err := m.storedJobs(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]ManagedJob, len(stored))
	for i, jc := range stored {
		out[i] = ManagedJob{JobConfig: jc, Error: m.invalid[kindJob][jc.Name]}
	}

	return out, nil
}

// CreateJob validates and stores a new job and activates it; without a
// start-time, it runs right away. Validation failures wrap ErrInvalid; a
// taken name returns store.ErrJobExists.
func (m *Manager) CreateJob(ctx context.Context, user string, fj config.FileJob) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	fj = normalizeJob(fj)

	m.mu.Lock()
	defer m.mu.Unlock()

	job, err := m.validateJobLocked(fj)
	if err != nil {
		return err
	}

	now := time.Now()
	jc := toStoreJob(fj)
	jc.CreatedAt, jc.CreatedBy, jc.UpdatedAt, jc.UpdatedBy = now, user, now, user

	if err := m.db.CreateJobConfig(ctx, jc); err != nil {
		return err
	}

	m.jobDefs[job.Name] = fj
	m.activateJobLocked(ctx, job.Name, true)

	if j, ok := m.jobs[job.Name]; ok && m.runCtx != nil {
		pipeline.WarnIfKeyWontChange(m.log, j)
	}

	m.log.Info("job created", "job", job.Name, "by", user)

	return nil
}

// UpdateJob validates and stores job fj.Name's new definition and applies
// it: from its next run on, or — when its interval or start-time changed —
// by rescheduling it (a run in flight finishes first). Validation failures
// wrap ErrInvalid; an unknown name returns store.ErrJobNotFound.
func (m *Manager) UpdateJob(ctx context.Context, user string, fj config.FileJob) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	fj = normalizeJob(fj)

	m.mu.Lock()
	defer m.mu.Unlock()

	job, err := m.validateJobLocked(fj)
	if err != nil {
		return err
	}

	jc := toStoreJob(fj)
	jc.UpdatedAt, jc.UpdatedBy = time.Now(), user

	if err := m.db.UpdateJobConfig(ctx, jc); err != nil {
		return err
	}

	m.jobDefs[job.Name] = fj
	m.activateJobLocked(ctx, job.Name, false)

	m.log.Info("job updated", "job", job.Name, "by", user)

	return nil
}

// DeleteJob removes job name: it's unscheduled (a run in flight finishes)
// and leaves the dashboard. Its run history and the backups it wrote are
// kept. An unknown name returns store.ErrJobNotFound.
func (m *Manager) DeleteJob(ctx context.Context, user, name string) error {
	if err := m.checkMutable(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.db.DeleteJobConfig(ctx, name); err != nil {
		return err
	}

	delete(m.jobDefs, name)
	delete(m.invalid[kindJob], name)
	m.deactivateJobLocked(name)

	m.log.Info("job deleted", "job", name, "by", user)

	return nil
}

// validateJobLocked resolves fj against the active servers and commands and
// checks its failure-notifications exist, wrapping any failure in
// ErrInvalid. m.mu must be held.
func (m *Manager) validateJobLocked(fj config.FileJob) (*config.Config, error) {
	job, err := config.ResolveJob(fj, m.settings.GPG, m.servers, m.commands, m.settings.ServerName)
	if err != nil {
		return nil, fmt.Errorf("%w job: %w", ErrInvalid, err)
	}

	if err := config.CheckNotificationRefs(fj.FailureNotifications, m.notifications); err != nil {
		return nil, fmt.Errorf("%w job: failure-notifications%w", ErrInvalid, err)
	}

	return job, nil
}

// usersLocked maps each server name (byServer) or else each command id to
// the stored jobs using it ("job <name>"). m.mu must be held.
func (m *Manager) usersLocked(byServer bool) map[string][]string {
	users := make(map[string][]string)
	add := func(key, job string) {
		if key != "" && !slices.Contains(users[key], "job "+job) {
			users[key] = append(users[key], "job "+job)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(m.jobDefs)) {
		for _, t := range m.jobDefs[name].Targets {
			switch {
			case byServer:
				add(strings.TrimSpace(t.Server), name)
			default:
				if t.OnError != nil {
					add(strings.TrimSpace(t.OnError.Command), name)
				}

				if t.OnRecover != nil {
					add(strings.TrimSpace(t.OnRecover.Command), name)
				}
			}
		}
	}

	return users
}
