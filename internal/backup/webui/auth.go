// This file implements the web UI's only login method, SSO as a bearer-token
// check: the SPA runs the OpenID Connect authorization-code-with-PKCE flow
// itself, as a public client (see frontend/src/auth/oidc.ts), and then sends
// the provider-issued access token (a JWT) as "Authorization: Bearer ..." on
// every /api/... request. This process never takes part in the provider's
// redirect flow, never sees an authorization code, and never holds a client
// secret — it only verifies each token it's handed (signature against the
// provider's JWKS, issuer, expiry, and that it was issued to the configured
// client) and turns it into a principal whose permissions come solely from
// the config file's webui.oidc.default-permissions plus the token's groups
// (see webui.oidc.group-permissions). Nothing about a user is stored
// locally.
//
// None of this applies to the receiver API (internal/backup/receiver),
// which shares the same HTTP server but authenticates each sending
// instance with its own per-receiver public-key-verified JWT.

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// oidcTimeout bounds each network round-trip to the OIDC provider
// (discovery, JWKS refresh) so an unreachable or slow IdP fails the request
// instead of hanging it indefinitely.
const oidcTimeout = 10 * time.Second

// loginMethodSSO is the login log's method for every entry auth.go records.
const loginMethodSSO = "oidc"

// errSSODisabled is returned by ssoBearerUser when a bearer token arrives
// while webui.oidc is switched off.
var errSSODisabled = errors.New("sso is not enabled")

type contextKey int

const userContextKey contextKey = iota

// principal is the signed-in user of one request, built from its SSO access
// token alone (see ssoBearerUser): Perm is webui.oidc.default-permissions
// plus what the token's groups grant via webui.oidc.group-permissions.
type principal struct {
	Username string
	Perm     permission.Permission
}

// currentUser returns the principal attached to ctx by requireUser, if any.
func currentUser(ctx context.Context) (*principal, bool) {
	u, ok := ctx.Value(userContextKey).(*principal)
	return u, ok
}

// authenticator bundles what requireUser needs to resolve a request to a
// principal: the SSO settings, the cache of OIDC verifiers, and what
// recordLogin needs for the login log. One is built per server in
// StartWebUI and shared by every route.
type authenticator struct {
	oidc              config.OIDCSettings
	verifiers         *ssoVerifierCache
	db                *store.Store
	log               *slog.Logger
	trustProxyHeaders bool
}

func newAuthenticator(oidcSettings config.OIDCSettings, db *store.Store, log *slog.Logger, trustProxyHeaders bool) *authenticator {
	return &authenticator{
		oidc:              oidcSettings,
		verifiers:         newSSOVerifierCache(),
		db:                db,
		log:               log,
		trustProxyHeaders: trustProxyHeaders,
	}
}

// requireUser resolves the request to a principal before calling next,
// storing it in the request context (see currentUser). The only accepted
// credential is an "Authorization: Bearer" SSO access token (see
// ssoBearerUser) — there are no local accounts or session cookies. A
// missing/invalid credential gets a 401; the SPA itself decides whether to
// renew its token or navigate to /login based on that.
func requireUser(a *authenticator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		user, err := ssoBearerUser(r.Context(), a.oidc, a.verifiers, raw)
		if err != nil {
			a.rejectBearer(w, r, err)
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	}
}

// requirePermission composes requireUser with check against the signed-in
// user's permissions, reporting 403 — the request is authenticated, just
// not authorized for this endpoint — if check returns false.
func requirePermission(a *authenticator, check func(permission.Permission) bool, next http.HandlerFunc) http.HandlerFunc {
	return requireUser(a, func(w http.ResponseWriter, r *http.Request) {
		user, _ := currentUser(r.Context())
		if !check(user.Perm) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		next(w, r)
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header (scheme matched case-insensitively, per RFC 7235).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}

	return strings.TrimSpace(token), true
}

// rejectBearer answers a failed bearer check with a generic 401 — the
// specific reason is only logged server-side. An expired token (a routine
// event for an idle tab, which the SPA recovers from by renewing it) is only
// logged at debug level; anything else (bad signature, wrong issuer/client,
// SSO disabled) is also recorded as a failed login in the login log.
func (a *authenticator) rejectBearer(w http.ResponseWriter, r *http.Request, err error) {
	if isTokenExpired(err) {
		a.log.Debug("sso: bearer token expired", "err", err)
	} else {
		a.log.Warn("sso: bearer token rejected", "err", err)
		recordLogin(r.Context(), a.db, a.log, r, a.trustProxyHeaders, loginMethodSSO, "sso", "", err.Error(), false)
	}

	http.Error(w, "invalid or expired token", http.StatusUnauthorized)
}

// ssoStatusJSON is what the public GET /api/sso/status returns: what the
// unauthenticated login page needs to show an SSO button and run the OIDC
// flow itself as a public client. The provider fields are only filled in
// while SSO is enabled; none of them are secret — they appear in every
// authorization redirect to the provider anyway.
type ssoStatusJSON struct {
	Enabled     bool   `json:"enabled"`
	ButtonLabel string `json:"buttonLabel"`
	IssuerURL   string `json:"issuerUrl,omitempty"`
	ClientID    string `json:"clientId,omitempty"`
	Scopes      string `json:"scopes,omitempty"`
}

// handleSSOStatus serves GET /api/sso/status. Deliberately not gated behind
// requireUser: the login page needs this before any credential exists.
func handleSSOStatus(s config.OIDCSettings) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !s.Enabled {
			writeJSON(w, ssoStatusJSON{})
			return
		}

		writeJSON(w, ssoStatusJSON{
			Enabled: true, ButtonLabel: s.ButtonLabel, IssuerURL: s.Issuer, ClientID: s.ClientID,
			Scopes: strings.Join(s.Scopes, " "),
		})
	}
}

// meJSON is what GET /api/me and POST /api/sso/login return: enough for the
// SPA to decide what to show/hide for the signed-in user. Admin mirrors
// permission.Permission.CanAdmin, the gate on job retry.
type meJSON struct {
	Username    string   `json:"username"`
	Permissions []string `json:"permissions"`
	Admin       bool     `json:"admin"`
}

func writeMe(w http.ResponseWriter, user *principal) {
	perms := user.Perm.Names()
	if perms == nil {
		perms = []string{}
	}

	writeJSON(w, meJSON{Username: user.Username, Permissions: perms, Admin: user.Perm.CanAdmin()})
}

// handleMe serves GET /api/me. It records nothing: the SPA calls it on
// every page load, so it can't double as the login audit point — see
// handleSSOLogin for that.
func handleMe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _ := currentUser(r.Context())
		writeMe(w, user)
	}
}

// handleSSOLogin serves POST /api/sso/login, the audit point for SSO
// logins: this server takes no part in the login itself (the SPA talks to
// the provider directly), so the SPA calls this exactly once, right after
// completing the provider's login, to record it in the login log. It
// returns the same body as GET /api/me so the callback page can load the
// account in the same round trip.
func handleSSOLogin(a *authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _ := currentUser(r.Context())
		recordLogin(r.Context(), a.db, a.log, r, a.trustProxyHeaders, loginMethodSSO, "sso", user.Username,
			"permissions="+strings.Join(user.Perm.Names(), ","), true)
		writeMe(w, user)
	}
}

// ssoVerifierCache holds one *oidc.IDTokenVerifier per issuer URL, built
// lazily on first use. Verification happens on every API request, so the
// discovery document and JWKS must be cached: the verifier wraps go-oidc's
// RemoteKeySet, which keeps the provider's keys in memory and only refetches
// them when it sees an unknown key ID. The SSO config is fixed for the
// process's lifetime, so in practice this only ever holds one entry; keying
// by issuer just keeps it trivially correct. A failed discovery is not
// cached, so an IdP that's down at startup is retried on the next request.
// The lock is held across discovery on purpose, so a burst of first
// requests triggers only one.
type ssoVerifierCache struct {
	mu        sync.Mutex
	verifiers map[string]*oidc.IDTokenVerifier
}

func newSSOVerifierCache() *ssoVerifierCache {
	return &ssoVerifierCache{verifiers: map[string]*oidc.IDTokenVerifier{}}
}

func (c *ssoVerifierCache) get(ctx context.Context, issuerURL string) (*oidc.IDTokenVerifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if v, ok := c.verifiers[issuerURL]; ok {
		return v, nil
	}

	// The http.Client (not just ctx's deadline) carries the timeout: go-oidc
	// keeps this context, minus its cancellation, for every later background
	// JWKS refresh.
	client := &http.Client{Timeout: oidcTimeout}
	discoverCtx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client), oidcTimeout)

	defer cancel()

	provider, err := oidc.NewProvider(discoverCtx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover oidc provider: %w", err)
	}

	// SkipClientIDCheck: an access token's "aud" is provider-specific (e.g.
	// Keycloak's default is "account", not the client ID), so go-oidc's
	// audience check would reject perfectly valid tokens. ssoBearerUser
	// binds the token to the configured client itself instead (see
	// tokenIssuedTo).
	v := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	c.verifiers[issuerURL] = v

	return v, nil
}

// bearerClaims are the access-token claims ssoBearerUser reads. Audience is
// a []string-or-string in the JWT spec; go-oidc normalizes it onto
// oidc.IDToken.Audience, so only azp needs decoding here.
type bearerClaims struct {
	AuthorizedParty   string `json:"azp"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
}

// ssoBearerUser verifies rawToken against s and returns the principal it
// identifies. Its permissions are s.DefaultPermissions plus whatever the
// token's groups are mapped to in s.GroupPermissions — worked out afresh on
// every request and never stored, so a group change at the provider applies
// on the user's next request.
func ssoBearerUser(ctx context.Context, s config.OIDCSettings, cache *ssoVerifierCache, rawToken string) (*principal, error) {
	if !s.Enabled {
		return nil, errSSODisabled
	}

	verifier, err := cache.get(ctx, s.Issuer)
	if err != nil {
		return nil, err
	}

	token, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("verify token: %w", err)
	}

	var claims bearerClaims
	if err := token.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}

	if !tokenIssuedTo(s.ClientID, claims.AuthorizedParty, token.Audience) {
		return nil, errors.New("token was not issued to the configured client")
	}

	var all map[string]json.RawMessage
	if err := token.Claims(&all); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}

	return &principal{
		Username: usernameFromClaims(claims.Email, claims.PreferredUsername, token.Subject),
		Perm:     s.DefaultPermissions | groupGrants(s.GroupPermissions, groupsFromClaim(all[s.GroupsClaim])),
	}, nil
}

// groupsFromClaim decodes a groups claim, which providers send either as a
// string array or, for a single group, as a plain string. A missing or
// differently shaped claim means no groups.
func groupsFromClaim(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var groups []string
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups
	}

	var group string
	if err := json.Unmarshal(raw, &group); err == nil && group != "" {
		return []string{group}
	}

	return nil
}

// groupGrants is the union of what every group in groups is mapped to.
// Groups without an entry grant nothing.
func groupGrants(mapping map[string]permission.Permission, groups []string) permission.Permission {
	var granted permission.Permission

	for _, g := range groups {
		granted |= mapping[g]
	}

	return granted
}

// tokenIssuedTo reports whether a token belongs to clientID: either its
// authorized party (azp — the client the token was issued to) or its
// audience names it. Without this, any token the same issuer minted for an
// unrelated client would be accepted here too.
func tokenIssuedTo(clientID, azp string, audience []string) bool {
	return azp == clientID || slices.Contains(audience, clientID)
}

// isTokenExpired reports whether err is (or wraps) go-oidc's expiry error —
// the one bearer failure that's routine (an idle browser tab) rather than
// security-relevant, so rejectBearer keeps it out of the login log.
func isTokenExpired(err error) bool {
	var expired *oidc.TokenExpiredError
	return errors.As(err, &expired)
}

// usernameFromClaims picks the name a principal is shown and audited as:
// preferred_username if present, else the email claim, else the subject
// identifier itself.
func usernameFromClaims(email, preferredUsername, subject string) string {
	switch {
	case preferredUsername != "":
		return preferredUsername
	case email != "":
		return email
	default:
		return subject
	}
}
