package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/jobs"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// maxJobConfigBody bounds a job/server/command create/update request body.
const maxJobConfigBody = 64 << 10

// auditJSON is every stored definition's created/updated wire fields.
type auditJSON struct {
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// jobConfigJSON is one stored job's wire shape for /api/job-configs (see
// jobs.ManagedJob).
type jobConfigJSON struct {
	config.FileJob
	auditJSON

	Error string `json:"error,omitempty"`
}

// serverOptionJSON is one server a job's targets may name, as the
// dashboard's edit form lists it.
type serverOptionJSON struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// jobConfigListJSON is GET /api/job-configs' response: every stored job,
// plus what the dashboard's edit form needs — whether editing is allowed,
// and the servers, command ids, and notification ids it may pick from.
type jobConfigListJSON struct {
	Editing       bool               `json:"editing"`
	Servers       []serverOptionJSON `json:"servers"`
	Commands      []string           `json:"commands"`
	Notifications []string           `json:"notifications"`
	Jobs          []jobConfigJSON    `json:"jobs"`
}

// serverConfigJSON is one stored server's wire shape for
// /api/server-configs (see jobs.ManagedServer).
type serverConfigJSON struct {
	config.FileServer
	auditJSON

	UsedBy []string `json:"used_by"`
	Error  string   `json:"error,omitempty"`
}

type serverConfigListJSON struct {
	Editing bool               `json:"editing"`
	Servers []serverConfigJSON `json:"servers"`
}

// commandConfigJSON is one stored command's wire shape for
// /api/command-configs (see jobs.ManagedCommand).
type commandConfigJSON struct {
	config.FileCommand
	auditJSON

	UsedBy []string `json:"used_by"`
	Error  string   `json:"error,omitempty"`
}

type commandConfigListJSON struct {
	Editing  bool                `json:"editing"`
	Commands []commandConfigJSON `json:"commands"`
}

// handleListJobConfigs serves GET /api/job-configs (admin only).
func handleListJobConfigs(m *jobs.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.ListJobs(r.Context())
		if err != nil {
			writeJobConfigError(w, log, "listing jobs", "", err)
			return
		}

		out := jobConfigListJSON{
			Editing: m.Editing(), Servers: []serverOptionJSON{}, Commands: nonNil(m.CommandIDs()),
			Notifications: nonNil(m.NotificationIDs()), Jobs: make([]jobConfigJSON, len(list)),
		}

		for _, s := range m.Servers() {
			out.Servers = append(out.Servers, serverOptionJSON{Name: s.Name, Type: string(s.Kind)})
		}

		for i, mj := range list {
			fj := jobs.FileJobFrom(mj.JobConfig)
			fj.Recipients, fj.FailureNotifications = nonNil(fj.Recipients), nonNil(fj.FailureNotifications)
			out.Jobs[i] = jobConfigJSON{FileJob: fj, auditJSON: audit(mj.CreatedAt, mj.CreatedBy, mj.UpdatedAt, mj.UpdatedBy), Error: mj.Error}
		}

		writeJSON(w, out)
	}
}

// handleListServerConfigs serves GET /api/server-configs (admin only).
func handleListServerConfigs(m *jobs.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.ListServers(r.Context())
		if err != nil {
			writeJobConfigError(w, log, "listing servers", "", err)
			return
		}

		out := serverConfigListJSON{Editing: m.Editing(), Servers: make([]serverConfigJSON, len(list))}
		for i, ms := range list {
			sc := ms.ServerConfig
			out.Servers[i] = serverConfigJSON{
				Name: sc.Name, Type: sc.Type, Endpoint: sc.Endpoint, Path: sc.Path, Retention: sc.Retention,
				auditJSON: audit(sc.CreatedAt, sc.CreatedBy, sc.UpdatedAt, sc.UpdatedBy),
				UsedBy:    nonNil(ms.UsedBy), Error: ms.Error,
			}
		}

		writeJSON(w, out)
	}
}

// handleListCommandConfigs serves GET /api/command-configs (admin only).
func handleListCommandConfigs(m *jobs.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.ListCommands(r.Context())
		if err != nil {
			writeJobConfigError(w, log, "listing commands", "", err)
			return
		}

		out := commandConfigListJSON{Editing: m.Editing(), Commands: make([]commandConfigJSON, len(list))}
		for i, mc := range list {
			cc := mc.CommandConfig
			out.Commands[i] = commandConfigJSON{
				FileCommand: jobs.FileCommandFrom(cc),
				auditJSON:   audit(cc.CreatedAt, cc.CreatedBy, cc.UpdatedAt, cc.UpdatedBy),
				UsedBy:      nonNil(mc.UsedBy), Error: mc.Error,
			}
		}

		writeJSON(w, out)
	}
}

func audit(createdAt time.Time, createdBy string, updatedAt time.Time, updatedBy string) auditJSON {
	return auditJSON{CreatedAt: createdAt, CreatedBy: createdBy, UpdatedAt: updatedAt, UpdatedBy: updatedBy}
}

// configMutation serves one kind of definition's create/update requests: T
// is decoded from the request body, key points at its identifying field (a
// job's or server's name, a command's id), and create/update are the
// jobs.Manager methods that apply it.
type configMutation[T any] struct {
	what           string
	key            func(*T) *string
	create, update func(m *jobs.Manager, ctx context.Context, user string, v T) error
	del            func(m *jobs.Manager, ctx context.Context, user, key string) error
}

var (
	jobMutation = configMutation[config.FileJob]{
		what: "job", key: func(v *config.FileJob) *string { return &v.Name },
		create: (*jobs.Manager).CreateJob, update: (*jobs.Manager).UpdateJob, del: (*jobs.Manager).DeleteJob,
	}
	serverMutation = configMutation[config.FileServer]{
		what: "server", key: func(v *config.FileServer) *string { return &v.Name },
		create: (*jobs.Manager).CreateServer, update: (*jobs.Manager).UpdateServer, del: (*jobs.Manager).DeleteServer,
	}
	commandMutation = configMutation[config.FileCommand]{
		what: "command", key: func(v *config.FileCommand) *string { return &v.ID },
		create: (*jobs.Manager).CreateCommand, update: (*jobs.Manager).UpdateCommand, del: (*jobs.Manager).DeleteCommand,
	}
)

// handleCreate serves POST /api/<what>-configs (admin only), answering 201.
func (c configMutation[T]) handleCreate(m *jobs.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var v T
		if !decodeJobConfig(w, r, &v) {
			return
		}

		user, _ := currentUser(r.Context())

		if err := c.create(m, r.Context(), user.Username, v); err != nil {
			writeJobConfigError(w, log, "creating "+c.what, *c.key(&v), err)
			return
		}

		w.WriteHeader(http.StatusCreated)
	}
}

// handleUpdate serves PUT /api/<what>-configs/{key} (admin only). The key
// is taken from the URL: it can't be changed.
func (c configMutation[T]) handleUpdate(m *jobs.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var v T
		if !decodeJobConfig(w, r, &v) {
			return
		}

		key := r.PathValue("key")
		if k := c.key(&v); *k != "" && *k != key {
			http.Error(w, "a "+c.what+"'s name can't be changed", http.StatusBadRequest)
			return
		}

		*c.key(&v) = key
		user, _ := currentUser(r.Context())

		if err := c.update(m, r.Context(), user.Username, v); err != nil {
			writeJobConfigError(w, log, "updating "+c.what, key, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleDelete serves DELETE /api/<what>-configs/{key} (admin only).
func (c configMutation[T]) handleDelete(m *jobs.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		user, _ := currentUser(r.Context())

		if err := c.del(m, r.Context(), user.Username, key); err != nil {
			writeJobConfigError(w, log, "deleting "+c.what, key, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeJobConfig(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJobConfigBody)).Decode(v); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}

	return true
}

// writeJobConfigError maps a jobs.Manager error onto its HTTP status, with
// validation messages passed through for the dashboard to show.
func writeJobConfigError(w http.ResponseWriter, log *slog.Logger, action, key string, err error) {
	switch {
	case errors.Is(err, jobs.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, jobs.ErrEditingDisabled):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, store.ErrJobNotFound), errors.Is(err, store.ErrServerNotFound), errors.Is(err, store.ErrCommandNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrJobExists), errors.Is(err, store.ErrServerExists), errors.Is(err, store.ErrCommandExists),
		errors.Is(err, jobs.ErrInUse):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, jobs.ErrStoreUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		log.Warn("web UI: "+action+" failed", "key", key, "err", err)
		http.Error(w, action+" failed", http.StatusInternalServerError)
	}
}

// registerJobConfigRoutes mounts /api/job-configs, /api/server-configs, and
// /api/command-configs on mux, each wrapped in admin. With no manager (e.g.
// a test that doesn't need them), nothing is mounted.
func registerJobConfigRoutes(mux *http.ServeMux, m *jobs.Manager, admin func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) {
	if m == nil {
		return
	}

	mux.HandleFunc("GET /api/job-configs", admin(handleListJobConfigs(m, log)))
	mux.HandleFunc("GET /api/server-configs", admin(handleListServerConfigs(m, log)))
	mux.HandleFunc("GET /api/command-configs", admin(handleListCommandConfigs(m, log)))

	registerMutationRoutes(mux, "/api/job-configs", jobMutation, m, admin, log)
	registerMutationRoutes(mux, "/api/server-configs", serverMutation, m, admin, log)
	registerMutationRoutes(mux, "/api/command-configs", commandMutation, m, admin, log)
}

func registerMutationRoutes[T any](mux *http.ServeMux, path string, c configMutation[T], m *jobs.Manager, admin func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) {
	mux.HandleFunc("POST "+path, admin(c.handleCreate(m, log)))
	mux.HandleFunc("PUT "+path+"/{key}", admin(c.handleUpdate(m, log)))
	mux.HandleFunc("DELETE "+path+"/{key}", admin(c.handleDelete(m, log)))
}
