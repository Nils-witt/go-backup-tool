package backup

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oidcAuth wraps everything the dashboard's SSO login (see
// handleOIDCLogin/handleOIDCCallback) needs once a provider has been
// discovered: the oauth2 client config used to build the authorization URL
// and exchange a code, and the ID token verifier checked against that same
// provider's JWKS.
type oidcAuth struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

// newOIDCAuth builds an *oidcAuth for cfg by fetching its provider's
// discovery document (issuer + "/.well-known/openid-configuration"), which
// requires a live network call — the reason this isn't done in config.go's
// parseFlags, whose errors are meant to be flag/config-shape problems, not
// network ones. Called once at startup (see runWithContext in app.go) when
// cfg.enabled; a returned error means the provider couldn't be reached or
// doesn't speak OIDC discovery, which the caller treats as a soft failure —
// logging a warning and running the web UI without SSO rather than failing
// the whole process over an IdP that's down.
func newOIDCAuth(ctx context.Context, cfg oidcSettings) (*oidcAuth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.issuer)
	if err != nil {
		return nil, err
	}

	return &oidcAuth{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.clientID,
			ClientSecret: cfg.clientSecret,
			RedirectURL:  cfg.redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       append([]string{oidc.ScopeOpenID}, cfg.scopes...),
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.clientID}),
	}, nil
}

// setupOIDCAuth builds an *oidcAuth for cfg (see newOIDCAuth) when
// cfg.enabled, treating a discovery failure as a soft failure: it logs a
// warning and returns nil, running the web UI without SSO rather than
// failing the whole process over an IdP that's unreachable at startup. A
// disabled cfg (the common case) is a silent no-op.
func setupOIDCAuth(ctx context.Context, cfg oidcSettings, log *slog.Logger) *oidcAuth {
	if !cfg.enabled {
		return nil
	}

	auth, err := newOIDCAuth(ctx, cfg)
	if err != nil {
		log.Warn("oidc: setting up provider failed, SSO login disabled", "issuer", cfg.issuer, "err", err)
		return nil
	}

	return auth
}

// oidcPendingTTL is how long an in-flight login (see oidcPendingStore)
// remains valid: long enough for a person to authenticate at the provider,
// short enough that an abandoned attempt's state doesn't linger.
const oidcPendingTTL = 10 * time.Minute

// oidcPending is what oidcPendingStore remembers about one in-flight SSO
// login, keyed by its state value: the nonce sent to the provider (and
// expected back in the ID token, guarding against token replay) and next
// (see safeNextPath), the page to return to once the login completes.
type oidcPending struct {
	nonce   string
	next    string
	expires time.Time
}

// oidcPendingStore tracks in-flight SSO logins (see handleOIDCLogin) between
// the redirect to the provider and its callback (see handleOIDCCallback),
// keyed by an unguessable per-attempt state value: the provider is expected
// to echo that value back unchanged, so its presence in this store (and not
// having expired) is what proves the callback corresponds to a login this
// instance actually started, standing in for the state cookie a
// browser-side CSRF check would otherwise need. Safe for concurrent use.
type oidcPendingStore struct {
	mu   sync.Mutex
	byID map[string]oidcPending
}

// newOIDCPendingStore returns an empty oidcPendingStore.
func newOIDCPendingStore() *oidcPendingStore {
	return &oidcPendingStore{byID: make(map[string]oidcPending)}
}

// start records a new in-flight login for next, returning its state and
// nonce (see oidcPending) for the caller to send to the provider.
func (s *oidcPendingStore) start(next string) (state, nonce string, err error) {
	state, err = randomSessionID()
	if err != nil {
		return "", "", err
	}

	nonce, err = randomSessionID()
	if err != nil {
		return "", "", err
	}

	s.mu.Lock()
	s.byID[state] = oidcPending{nonce: nonce, next: next, expires: time.Now().Add(oidcPendingTTL)}
	s.mu.Unlock()

	return state, nonce, nil
}

// consume looks up and removes the pending login for state — a callback
// only ever legitimately presents one state once — reporting ok=false if
// state is unknown or has expired.
func (s *oidcPendingStore) consume(state string) (oidcPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.byID[state]
	delete(s.byID, state)

	if !ok || time.Now().After(p.expires) {
		return oidcPending{}, false
	}

	return p, true
}

// handleOIDCLogin serves GET /login/oidc: it starts a new in-flight login
// (see oidcPendingStore) for next (see safeNextPath) and redirects the
// browser to auth's provider to authenticate.
func handleOIDCLogin(auth *oidcAuth, pending *oidcPendingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next := safeNextPath(r.URL.Query().Get("next"))

		state, nonce, err := pending.start(next)
		if err != nil {
			http.Error(w, "starting login failed", http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, auth.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce)), http.StatusSeeOther)
	}
}

// handleOIDCCallback serves GET /login/oidc/callback, the redirect target
// the provider sends the browser back to once it's authenticated: it
// resolves the in-flight login named by the callback's state parameter (see
// oidcPendingStore.consume), exchanges the authorization code for tokens,
// verifies the ID token (signature, issuer, audience — see
// oidc.IDTokenVerifier.Verify — plus the nonce, matched against the
// in-flight login's own, guarding against a replayed token), and — once all
// of that checks out — starts a dashboard session exactly as a successful
// password login would (see handleWebUILogin), redirecting the browser to
// the in-flight login's next. db, when non-nil, gets every attempt appended
// to the login log (see recordLoginEvent), win or lose, mirroring
// handleWebUILogin's own recording.
func handleOIDCCallback(auth *oidcAuth, pending *oidcPendingStore, sessions *sessionStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record := func(username, detail string, success bool) {
			if db == nil {
				return
			}

			ev := loginEvent{At: time.Now(), Username: username, Method: "oidc", Success: success, RemoteAddr: r.RemoteAddr, Detail: detail}
			if err := recordLoginEvent(r.Context(), db, ev); err != nil {
				log.Warn("oidc: recording login event failed", "err", err)
			}
		}

		p, ok := pending.consume(r.URL.Query().Get("state"))
		if !ok {
			http.Error(w, "login expired or invalid, please try again", http.StatusBadRequest)
			return
		}

		if errParam := r.URL.Query().Get("error"); errParam != "" {
			log.Warn("oidc: provider returned an error", "error", errParam, "description", r.URL.Query().Get("error_description"))
			record("", "provider error: "+errParam, false)
			http.Error(w, "login failed at identity provider", http.StatusBadGateway)

			return
		}

		token, err := auth.oauth2Config.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			log.Warn("oidc: exchanging code failed", "err", err)
			record("", "exchanging code failed", false)
			http.Error(w, "login failed", http.StatusBadGateway)

			return
		}

		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			log.Warn("oidc: token response had no id_token")
			record("", "no id_token in response", false)
			http.Error(w, "login failed", http.StatusBadGateway)

			return
		}

		idToken, err := auth.verifier.Verify(r.Context(), rawIDToken)
		if err != nil {
			log.Warn("oidc: verifying id token failed", "err", err)
			record("", "verifying id token failed", false)
			http.Error(w, "login failed", http.StatusBadGateway)

			return
		}

		if idToken.Nonce != p.nonce {
			log.Warn("oidc: id token nonce did not match")
			record(oidcIdentity(idToken), "nonce did not match", false)
			http.Error(w, "login failed", http.StatusBadGateway)

			return
		}

		id, err := sessions.create()
		if err != nil {
			record(oidcIdentity(idToken), "starting session failed", false)
			http.Error(w, "starting session failed", http.StatusInternalServerError)

			return
		}

		record(oidcIdentity(idToken), "", true)
		http.SetCookie(w, sessions.cookie(id, r.TLS != nil))
		http.Redirect(w, r, p.next, http.StatusSeeOther)
	}
}

// oidcIdentity returns the best-effort human identity to record for a
// verified idToken (see handleOIDCCallback's login log entries): its "email"
// claim if the provider sent one, otherwise its subject.
func oidcIdentity(idToken *oidc.IDToken) string {
	var claims struct {
		Email string `json:"email"`
	}

	if err := idToken.Claims(&claims); err == nil && claims.Email != "" {
		return claims.Email
	}

	return idToken.Subject
}
