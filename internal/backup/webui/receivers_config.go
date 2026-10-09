package webui

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// maxReceiverConfigBody bounds a receiver create/update request body: a PEM
// RSA public key is a few hundred bytes, so this leaves plenty of room.
const maxReceiverConfigBody = 64 << 10

// receiverConfigJSON is one stored receiver's wire shape for
// /api/receiver-configs (see receiver.ManagedReceiver).
type receiverConfigJSON struct {
	config.FileReceiver

	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
	Error     string    `json:"error,omitempty"`
}

// trustedServerOptionJSON is one trusted server a receiver may allow, as
// the dashboard's edit form lists it.
type trustedServerOptionJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// receiverConfigListJSON is GET /api/receiver-configs' response: every
// stored receiver, plus what the dashboard's edit form needs — the base dir
// paths must lie inside, and the notification ids and trusted servers it
// may pick from.
type receiverConfigListJSON struct {
	BaseDir        string                    `json:"base_dir"`
	Notifications  []string                  `json:"notifications"`
	TrustedServers []trustedServerOptionJSON `json:"trusted_servers"`
	Receivers      []receiverConfigJSON      `json:"receivers"`
}

// handleListReceiverConfigs serves GET /api/receiver-configs (admin only).
func handleListReceiverConfigs(m *receiver.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.List(r.Context())
		if err != nil {
			writeReceiverConfigError(w, log, "listing", "", err)
			return
		}

		out := receiverConfigListJSON{BaseDir: m.BaseDir(), Notifications: m.NotificationIDs(), Receivers: make([]receiverConfigJSON, len(list))}

		for _, s := range m.TrustedServers() {
			out.TrustedServers = append(out.TrustedServers, trustedServerOptionJSON{ID: s.ID, Name: s.Name})
		}

		if out.TrustedServers == nil {
			out.TrustedServers = []trustedServerOptionJSON{}
		}

		for i, mr := range list {
			rc := mr.ReceiverConfig
			out.Receivers[i] = receiverConfigJSON{
				ID: rc.ID, PublicKey: rc.PublicKey, AllowedServers: nonNil(rc.AllowedServers), Path: rc.Path, Retention: rc.Retention, StaleAfter: rc.StaleAfter,
				StaleNotifications: nonNil(rc.StaleNotifications), DownloadNotifications: nonNil(rc.DownloadNotifications),
				CreatedAt: rc.CreatedAt, CreatedBy: rc.CreatedBy, UpdatedAt: rc.UpdatedAt, UpdatedBy: rc.UpdatedBy,
				Error: mr.Error,
			}
		}

		writeJSON(w, out)
	}
}

// handleCreateReceiverConfig serves POST /api/receiver-configs (admin only),
// answering 201 on success.
func handleCreateReceiverConfig(m *receiver.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fr, ok := decodeReceiverConfig(w, r)
		if !ok {
			return
		}

		user, _ := currentUser(r.Context())

		if err := m.Create(r.Context(), user.Username, fr); err != nil {
			writeReceiverConfigError(w, log, "creating", fr.ID, err)
			return
		}

		w.WriteHeader(http.StatusCreated)
	}
}

// handleUpdateReceiverConfig serves PUT /api/receiver-configs/{id} (admin
// only). The id is taken from the URL; a receiver's id can't be changed.
func handleUpdateReceiverConfig(m *receiver.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fr, ok := decodeReceiverConfig(w, r)
		if !ok {
			return
		}

		id := r.PathValue("id")
		if fr.ID != "" && fr.ID != id {
			http.Error(w, "a receiver's id can't be changed", http.StatusBadRequest)
			return
		}

		fr.ID = id
		user, _ := currentUser(r.Context())

		if err := m.Update(r.Context(), user.Username, fr); err != nil {
			writeReceiverConfigError(w, log, "updating", id, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleDeleteReceiverConfig serves DELETE /api/receiver-configs/{id} (admin
// only). Files on disk are kept.
func handleDeleteReceiverConfig(m *receiver.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		user, _ := currentUser(r.Context())

		if err := m.Delete(r.Context(), user.Username, id); err != nil {
			writeReceiverConfigError(w, log, "deleting", id, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeReceiverConfig(w http.ResponseWriter, r *http.Request) (config.FileReceiver, bool) {
	var fr config.FileReceiver

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReceiverConfigBody)).Decode(&fr); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return config.FileReceiver{}, false
	}

	return fr, true
}

// writeReceiverConfigError maps a receiver.Manager error onto its HTTP
// status, with validation messages passed through for the dashboard to
// show.
func writeReceiverConfigError(w http.ResponseWriter, log *slog.Logger, action, id string, err error) {
	switch {
	case errors.Is(err, receiver.ErrInvalidReceiver):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrReceiverNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrReceiverExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, receiver.ErrReceiverStoreUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		log.Warn("web UI: "+action+" receiver failed", "id", id, "err", err)
		http.Error(w, action+" receiver failed", http.StatusInternalServerError)
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}
