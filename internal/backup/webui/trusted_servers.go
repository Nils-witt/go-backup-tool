package webui

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// maxTrustedServerBody bounds a trusted server create/update request body:
// a PEM RSA public key is well under a kilobyte.
const maxTrustedServerBody = 64 << 10

// trustedServerJSON is one stored trusted server's wire shape for
// /api/trusted-servers (see trust.ManagedServer).
type trustedServerJSON struct {
	trust.Input

	Fingerprint string    `json:"fingerprint"`
	UsedBy      []string  `json:"used_by"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by"`
	Error       string    `json:"error,omitempty"`
}

// trustedServerListJSON is GET /api/trusted-servers' response.
type trustedServerListJSON struct {
	Servers []trustedServerJSON `json:"servers"`
}

// handleListTrustedServers serves GET /api/trusted-servers (admin only).
func handleListTrustedServers(m *trust.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.List(r.Context())
		if err != nil {
			writeTrustedServerError(w, log, "listing", "", err)
			return
		}

		out := trustedServerListJSON{Servers: make([]trustedServerJSON, len(list))}

		for i, ms := range list {
			ts := ms.TrustedServerConfig
			out.Servers[i] = trustedServerJSON{
				ID: ts.ID, Name: ts.Name, PublicKey: ts.PublicKey,
				Fingerprint: ms.Fingerprint, UsedBy: nonNil(ms.UsedBy),
				CreatedAt: ts.CreatedAt, CreatedBy: ts.CreatedBy, UpdatedAt: ts.UpdatedAt, UpdatedBy: ts.UpdatedBy,
				Error: ms.Error,
			}
		}

		writeJSON(w, out)
	}
}

// handleCreateTrustedServer serves POST /api/trusted-servers (admin only),
// answering 201 on success.
func handleCreateTrustedServer(m *trust.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeTrustedServer(w, r)
		if !ok {
			return
		}

		user, _ := currentUser(r.Context())

		if err := m.Create(r.Context(), user.Username, in); err != nil {
			writeTrustedServerError(w, log, "creating", in.ID, err)
			return
		}

		w.WriteHeader(http.StatusCreated)
	}
}

// handleUpdateTrustedServer serves PUT /api/trusted-servers/{id} (admin
// only). The id is taken from the URL; a trusted server's id can't be
// changed, since it's the sending instance's own server UUID.
func handleUpdateTrustedServer(m *trust.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, ok := decodeTrustedServer(w, r)
		if !ok {
			return
		}

		id := trust.NormalizeID(r.PathValue("id"))
		if in.ID != "" && trust.NormalizeID(in.ID) != id {
			http.Error(w, "a trusted server's id can't be changed", http.StatusBadRequest)
			return
		}

		in.ID = id
		user, _ := currentUser(r.Context())

		if err := m.Update(r.Context(), user.Username, in); err != nil {
			writeTrustedServerError(w, log, "updating", id, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleDeleteTrustedServer serves DELETE /api/trusted-servers/{id} (admin
// only), refusing with 409 while a receiver still allows it.
func handleDeleteTrustedServer(m *trust.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := trust.NormalizeID(r.PathValue("id"))
		user, _ := currentUser(r.Context())

		if err := m.Delete(r.Context(), user.Username, id); err != nil {
			writeTrustedServerError(w, log, "deleting", id, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeTrustedServer(w http.ResponseWriter, r *http.Request) (trust.Input, bool) {
	var in trust.Input

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTrustedServerBody)).Decode(&in); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return trust.Input{}, false
	}

	return in, true
}

// trustedServerErrorStatus lists the trust.Manager errors answered with
// their own HTTP status rather than a logged 500.
var trustedServerErrorStatus = []struct {
	err    error
	status int
}{
	{trust.ErrInvalid, http.StatusBadRequest},
	{store.ErrTrustedServerNotFound, http.StatusNotFound},
	{store.ErrTrustedServerExists, http.StatusConflict},
	{trust.ErrInUse, http.StatusConflict},
	{trust.ErrStoreUnavailable, http.StatusServiceUnavailable},
}

// writeTrustedServerError maps a trust.Manager error onto its HTTP status
// (see trustedServerErrorStatus), with validation messages passed through
// for the dashboard to show; anything else is logged and answered 500.
func writeTrustedServerError(w http.ResponseWriter, log *slog.Logger, action, id string, err error) {
	for _, e := range trustedServerErrorStatus {
		if errors.Is(err, e.err) {
			http.Error(w, err.Error(), e.status)
			return
		}
	}

	log.Warn("web UI: "+action+" trusted server failed", "id", id, "err", err)
	http.Error(w, action+" trusted server failed", http.StatusInternalServerError)
}
