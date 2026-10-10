// This file implements the web UI's long-lived, read-only API tokens: named
// JWTs an admin issues from the dashboard for scripts and monitoring, each
// with a fixed lifetime and individually revocable. A token is HS256-signed
// with a key kept in the state db (see store.TokenSigningKey) and carries
// only its id (jti), issuer and expiry; every request still looks its id up
// in the state db, so revoking one takes effect immediately. A token's
// principal is only ever granted permission.PermissionView and the job run
// log (so a remote dashboard can draw each job's run history) — never
// download, admin, or any of the other audit logs.

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

const (
	// apiTokenIssuer is the iss claim of every API token this instance
	// signs, and what apiTokenUser requires.
	apiTokenIssuer = "go-backup-tool"

	// apiTokenPermissions is what every API token grants: read-only
	// dashboard access, without file downloads. The job run log is
	// included so a dashboard merging this instance in as a remote backend
	// can draw its jobs' run history.
	apiTokenPermissions = permission.PermissionView | permission.PermissionViewJobRunLog

	// apiTokenUsernamePrefix prefixes an API token's name to form its
	// principal's username, so it can't be mistaken for an SSO user.
	apiTokenUsernamePrefix = "token:"

	// loginMethodAPIToken is the login log's method for a rejected API token.
	loginMethodAPIToken = "api-token"

	// maxAPITokenLifetimeDays and maxAPITokenNameLen bound handleCreateAPIToken's
	// input.
	maxAPITokenLifetimeDays = 3650
	maxAPITokenNameLen      = 100
)

var (
	errAPITokenUnknown = errors.New("api token is unknown")
	errAPITokenRevoked = errors.New("api token has been revoked")
	errAPITokenExpired = errors.New("api token has expired")
)

// apiTokens signs and verifies API tokens. nil (no state db, or its signing
// key couldn't be loaded) disables them: requireUser then never treats a
// bearer token as one, and the token endpoints report 503.
type apiTokens struct {
	key []byte
	db  *store.Store
}

// newAPITokens loads db's signing key, returning nil (tokens disabled) if
// db is nil or the key can't be loaded.
func newAPITokens(ctx context.Context, db *store.Store, log *slog.Logger) *apiTokens {
	if db == nil {
		return nil
	}

	key, err := db.TokenSigningKey(ctx)
	if err != nil {
		log.Warn("web UI: loading api token signing key failed, api tokens are disabled", "err", err)
		return nil
	}

	return &apiTokens{key: key, db: db}
}

// sign returns the signed JWT for the token with the given id and expiry.
func (t *apiTokens) sign(id string, issuedAt, expiresAt time.Time) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: t.key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("building signer: %w", err)
	}

	claims := jwt.Claims{
		ID:       id,
		Issuer:   apiTokenIssuer,
		IssuedAt: jwt.NewNumericDate(issuedAt),
		Expiry:   jwt.NewNumericDate(expiresAt),
	}

	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("serializing token: %w", err)
	}

	return raw, nil
}

// parse reports whether raw is shaped like one of this instance's API
// tokens — a JWT signed with HS256, which no OIDC provider's JWKS-verified
// access token ever is — so requireUser knows which verifier to hand it to.
func (t *apiTokens) parse(raw string) (*jwt.JSONWebToken, bool) {
	if t == nil {
		return nil, false
	}

	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, false
	}

	return token, true
}

// user verifies token (as returned by parse) and returns its principal: the
// signature, issuer and expiry must check out, and its id must name a token
// in the state db that is neither revoked nor past its stored expiry.
func (t *apiTokens) user(ctx context.Context, token *jwt.JSONWebToken) (*principal, error) {
	var claims jwt.Claims
	if err := token.Claims(t.key, &claims); err != nil {
		return nil, fmt.Errorf("verify api token: %w", err)
	}

	if err := claims.ValidateWithLeeway(jwt.Expected{Issuer: apiTokenIssuer, Time: time.Now()}, 0); err != nil {
		if errors.Is(err, jwt.ErrExpired) {
			return nil, errAPITokenExpired
		}

		return nil, fmt.Errorf("validate api token: %w", err)
	}

	rec, found, err := t.db.GetAPIToken(ctx, claims.ID)
	if err != nil {
		return nil, err
	}

	switch {
	case !found:
		return nil, errAPITokenUnknown
	case rec.RevokedAt != nil:
		return nil, fmt.Errorf("%w (%s)", errAPITokenRevoked, rec.Name)
	case !time.Now().Before(rec.ExpiresAt):
		return nil, errAPITokenExpired
	}

	return &principal{Username: apiTokenUsernamePrefix + rec.Name, Perm: apiTokenPermissions}, nil
}

// apiTokenJSON is one API token's wire shape for GET /api/tokens, without
// the signed token itself (only handleCreateAPIToken's response has that).
type apiTokenJSON struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Permissions []string   `json:"permissions"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	RevokedBy   string     `json:"revoked_by,omitempty"`
}

func apiTokenToJSON(t store.APIToken) apiTokenJSON {
	return apiTokenJSON{
		ID: t.ID, Name: t.Name, Permissions: apiTokenPermissions.Names(),
		CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
		RevokedAt: t.RevokedAt, RevokedBy: t.RevokedBy,
	}
}

// createdAPITokenJSON is handleCreateAPIToken's response: the new token's
// metadata plus the signed token, which is never shown again.
type createdAPITokenJSON struct {
	apiTokenJSON

	Token string `json:"token"`
}

// createAPITokenRequest is handleCreateAPIToken's request body.
type createAPITokenRequest struct {
	Name         string `json:"name"`
	LifetimeDays int    `json:"lifetime_days"`
}

// tokensUnavailable answers a token endpoint while API tokens are disabled.
func tokensUnavailable(w http.ResponseWriter) {
	http.Error(w, "api tokens are unavailable (no state database)", http.StatusServiceUnavailable)
}

// handleListAPITokens serves GET /api/tokens: every issued API token,
// revoked and expired ones included, newest first.
func handleListAPITokens(tokens *apiTokens, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			writeJSON(w, []apiTokenJSON{})
			return
		}

		list, err := tokens.db.ListAPITokens(r.Context())
		if err != nil {
			log.Warn("web UI: listing api tokens", "err", err)
			http.Error(w, "listing api tokens failed", http.StatusInternalServerError)

			return
		}

		out := make([]apiTokenJSON, len(list))
		for i, t := range list {
			out[i] = apiTokenToJSON(t)
		}

		writeJSON(w, out)
	}
}

// handleCreateAPIToken serves POST /api/tokens: issues a new read-only API
// token with the requested name and lifetime, returning it signed — the
// only time the signed token is ever available.
func handleCreateAPIToken(tokens *apiTokens, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			tokensUnavailable(w)
			return
		}

		var req createAPITokenRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		req.Name = strings.TrimSpace(req.Name)

		switch {
		case req.Name == "":
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		case len(req.Name) > maxAPITokenNameLen:
			http.Error(w, fmt.Sprintf("name must be at most %d characters", maxAPITokenNameLen), http.StatusBadRequest)
			return
		case req.LifetimeDays < 1 || req.LifetimeDays > maxAPITokenLifetimeDays:
			http.Error(w, fmt.Sprintf("lifetime_days must be between 1 and %d", maxAPITokenLifetimeDays), http.StatusBadRequest)
			return
		}

		user, _ := currentUser(r.Context())
		now := time.Now().UTC().Truncate(time.Second)

		rec := store.APIToken{
			ID: uuid.NewString(), Name: req.Name, CreatedBy: user.Username,
			CreatedAt: now, ExpiresAt: now.AddDate(0, 0, req.LifetimeDays),
		}

		raw, err := tokens.sign(rec.ID, rec.CreatedAt, rec.ExpiresAt)
		if err != nil {
			log.Warn("web UI: signing api token", "err", err)
			http.Error(w, "creating api token failed", http.StatusInternalServerError)

			return
		}

		if err := tokens.db.SaveAPIToken(r.Context(), rec); err != nil {
			log.Warn("web UI: saving api token", "err", err)
			http.Error(w, "creating api token failed", http.StatusInternalServerError)

			return
		}

		log.Info("web UI: api token created", "id", rec.ID, "name", rec.Name, "by", rec.CreatedBy, "expires", rec.ExpiresAt)

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, createdAPITokenJSON{apiTokenJSON: apiTokenToJSON(rec), Token: raw})
	}
}

// handleRevokeAPIToken serves DELETE /api/tokens/{id}: revokes the token,
// effective on its very next request. The token's row is kept (marked
// revoked) so the list still shows it.
func handleRevokeAPIToken(tokens *apiTokens, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tokens == nil {
			tokensUnavailable(w)
			return
		}

		id := r.PathValue("id")
		user, _ := currentUser(r.Context())

		ok, err := tokens.db.RevokeAPIToken(r.Context(), id, user.Username, time.Now())
		if err != nil {
			log.Warn("web UI: revoking api token", "id", id, "err", err)
			http.Error(w, "revoking api token failed", http.StatusInternalServerError)

			return
		}

		if !ok {
			http.Error(w, "unknown or already revoked api token", http.StatusNotFound)
			return
		}

		log.Info("web UI: api token revoked", "id", id, "by", user.Username)

		w.WriteHeader(http.StatusNoContent)
	}
}
