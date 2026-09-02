package webui

import (
	"context"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// OIDCAuth wraps everything the dashboard's SSO login (see
// handleOIDCLogin/handleOIDCCallback) needs once a provider has been
// discovered: the oauth2 client config used to build the authorization URL
// and exchange a code, and the ID token verifier checked against that same
// provider's JWKS.
type OIDCAuth struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	defaultPerm  permission.Permission
}

// newOIDCAuth builds an *OIDCAuth for cfg by fetching its provider's
// discovery document (issuer + "/.well-known/openid-configuration"), which
// requires a live network call — the reason this isn't done in config.go's
// ParseFlags, whose errors are meant to be flag/config-shape problems, not
// network ones. Called once at startup (see runWithContext in app.go) when
// cfg.Enabled; a returned error means the provider couldn't be reached or
// doesn't speak OIDC discovery, which the caller treats as a soft failure —
// logging a warning and running the web UI without SSO rather than failing
// the whole process over an IdP that's down.
func newOIDCAuth(ctx context.Context, cfg config.OIDCSettings) (*OIDCAuth, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}

	return &OIDCAuth{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       append([]string{oidc.ScopeOpenID}, cfg.Scopes...),
		},
		verifier:    provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		defaultPerm: cfg.DefaultPermissions,
	}, nil
}

// SetupOIDCAuth builds an *OIDCAuth for cfg (see newOIDCAuth) when
// cfg.Enabled, treating a discovery failure as a soft failure: it logs a
// warning and returns nil, running the web UI without SSO rather than
// failing the whole process over an IdP that's unreachable at startup. A
// disabled cfg (the common case) is a silent no-op.
func SetupOIDCAuth(ctx context.Context, cfg config.OIDCSettings, log *slog.Logger) *OIDCAuth {
	if !cfg.Enabled {
		return nil
	}

	auth, err := newOIDCAuth(ctx, cfg)
	if err != nil {
		log.Warn("oidc: setting up provider failed, SSO login disabled", "issuer", cfg.Issuer, "err", err)
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
func handleOIDCLogin(auth *OIDCAuth, pending *oidcPendingStore) http.HandlerFunc {
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

// oidcCompleteTemplateSrc is the tiny bridge page handleOIDCCallback serves
// once a login succeeds: the callback is a top-level browser redirect
// landing, not a fetch() call the dashboard's own JS can read a token out
// of (see handleWebUILogin's JSON response for that path), so instead this
// page's inline <script> stores the freshly minted token itself and sends
// the browser on its way — see writeOIDCCompletePage.
//
//go:embed oidc_complete.html
var oidcCompleteTemplateSrc string

// oidcCompleteTemplate is oidcCompleteTemplateSrc parsed once at package
// init. html/template's contextual autoescaping JS-escapes Token/Next for
// their <script> string-literal context automatically, the same way
// loginPageTemplate escapes Next for its HTML attribute/URL contexts.
var oidcCompleteTemplate = template.Must(template.New("oidc_complete.html").Parse(oidcCompleteTemplateSrc))

// oidcCompleteData is oidcCompleteTemplate's input.
type oidcCompleteData struct {
	Token     string
	ExpiresAt string
	Next      string
}

// writeOIDCCompletePage renders oidcCompleteTemplate to w for a session that
// just started with token, expiring at expiresAt, sending the browser on to
// next once the token is stored client-side.
func writeOIDCCompletePage(w http.ResponseWriter, token string, expiresAt time.Time, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := oidcCompleteData{Token: token, ExpiresAt: expiresAt.Format(time.RFC3339), Next: next}
	if err := oidcCompleteTemplate.Execute(w, data); err != nil {
		// oidcCompleteTemplate is a fixed, compile-time-checked template
		// executed against a plain struct of strings, so this can't fail in
		// practice; panicking here would be worse than a broken page for an
		// otherwise-successful login.
		return
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
// password login would (see handleWebUILogin), then serves the bridge page
// (see writeOIDCCompletePage) that hands the browser its token and sends it
// on to the in-flight login's next. The session's permissions are
// auth.defaultPerm, unless db (when non-nil) already has a users row linked
// to this identity (see store.User.OIDCUsername, matched only via
// db.GetOrProvisionOIDCUser — never by comparing the identity string to an
// unrelated row's username) — set through the "Users" admin section (see
// handleUpdateUser in webui.go) — in which case that row's stored
// permissions take precedence. The very first login for an identity has no
// such row yet, so GetOrProvisionOIDCUser also provisions one then,
// granting auth.defaultPerm — the identity shows up in that admin listing
// from then on, ready for an admin to adjust or link to an existing
// password account, rather than only appearing once they've manually added
// it. db also, independently of all that, gets every attempt appended to
// the login log (see recordLoginEvent), win or lose, mirroring
// handleWebUILogin's own recording.
func handleOIDCCallback(auth *OIDCAuth, pending *oidcPendingStore, sessions *sessionStore, log *slog.Logger, db *store.Store, trustProxyHeaders bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record := func(username, detail string, success bool) {
			recordLogin(r.Context(), db, log, r, trustProxyHeaders, "oidc", "oidc", username, detail, success)
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

		identity := oidcIdentity(idToken)
		perm := auth.defaultPerm

		if db != nil {
			if p, err := db.GetOrProvisionOIDCUser(r.Context(), identity, auth.defaultPerm); err != nil {
				log.Warn("oidc: looking up/provisioning user record failed", "err", err)
			} else {
				perm = p
			}
		}

		id, err := sessions.create(identity, perm)
		if err != nil {
			record(identity, "starting session failed", false)
			http.Error(w, "starting session failed", http.StatusInternalServerError)

			return
		}

		record(identity, "", true)
		writeOIDCCompletePage(w, id, time.Now().Add(sessionTTL), p.next)
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
