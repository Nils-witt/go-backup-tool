package webui

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/gpgkeys"
)

// maxGPGKeyBody bounds a POST /api/gpg-keys request body (see
// gpgkeys.Keyring.Import's own limit on the key itself).
const maxGPGKeyBody = 512 << 10

// gpgKeyListJSON is GET /api/gpg-keys' response: every public key in the
// keyring jobs encrypt to, which homedir that is ("" for gpg's default),
// and whether keys may be added (see registerGPGKeyRoutes).
type gpgKeyListJSON struct {
	Homedir string        `json:"homedir"`
	Editing bool          `json:"editing"`
	Keys    []gpgkeys.Key `json:"keys"`
}

// gpgKeyImportJSON is POST /api/gpg-keys' request body.
type gpgKeyImportJSON struct {
	Armored string `json:"armored"`
}

// registerGPGKeyRoutes mounts /api/gpg-keys on mux: the listing wrapped in
// admin, an import in change (admin plus the audit log).
// Adding a key is only allowed with editing (webui.job-editing): jobs often
// name a recipient by email and encrypt with --trust-model always, so a new
// key carrying a recipient's address could receive that job's backups —
// as much a change to the jobs as editing them. With no keyring (e.g. a
// test that doesn't need it), nothing is mounted.
func registerGPGKeyRoutes(mux *http.ServeMux, keyring *gpgkeys.Keyring, editing bool, admin, change func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) {
	if keyring == nil {
		return
	}

	mux.HandleFunc("GET /api/gpg-keys", admin(handleListGPGKeys(keyring, editing, log)))
	mux.HandleFunc("POST /api/gpg-keys", change(handleImportGPGKey(keyring, editing, log)))
}

// handleListGPGKeys serves GET /api/gpg-keys (admin only).
func handleListGPGKeys(keyring *gpgkeys.Keyring, editing bool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := keyring.List(r.Context())
		if err != nil {
			log.Warn("web UI: listing gpg keys failed", "err", err)
			http.Error(w, "listing gpg keys failed", http.StatusInternalServerError)

			return
		}

		writeJSON(w, gpgKeyListJSON{Homedir: keyring.Homedir, Editing: editing, Keys: keys})
	}
}

// handleImportGPGKey serves POST /api/gpg-keys (admin only, and only with
// editing), answering 201 with what was imported.
func handleImportGPGKey(keyring *gpgkeys.Keyring, editing bool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !editing {
			http.Error(w, "adding gpg keys is disabled: set webui.job-editing: true in the config file to allow it", http.StatusForbidden)
			return
		}

		var in gpgKeyImportJSON
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGPGKeyBody)).Decode(&in); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		res, err := keyring.Import(r.Context(), in.Armored)
		if err != nil {
			if errors.Is(err, gpgkeys.ErrInvalidKey) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			log.Warn("web UI: importing gpg key failed", "err", err)
			http.Error(w, "importing gpg key failed", http.StatusInternalServerError)

			return
		}

		user, _ := currentUser(r.Context())
		log.Info("gpg key imported", "fingerprints", res.Fingerprints, "warnings", len(res.Warnings), "by", user.Username)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(res)
	}
}
