// This file implements OpenID Connect Back-Channel Logout 1.0
// (https://openid.net/specs/openid-connect-backchannel-1_0.html): when a
// user signs out at the provider, or an admin ends their session there, the
// provider POSTs a signed logout token to POST /api/sso/backchannel-logout.
//
// The dashboard's access tokens are verified statelessly (see auth.go), so
// the provider ending a session doesn't by itself stop an access token it
// already issued from working until it expires. Each accepted logout is
// therefore remembered — in memory, and in the state db so a restart keeps
// it — for ssoLogoutRetention, and ssoBearerUser rejects any access token
// it covers. An open live status WebSocket (see handleLive) of a covered
// session is closed too.
//
// A logout token naming a session ("sid") ends that session: access tokens
// carrying that sid are rejected, and so are the subject's access tokens
// without any sid issued before the logout, since nothing tells which
// session those belong to. A logout token with only a subject ("sub") ends
// all of that subject's sessions: every access token of theirs issued
// before the logout is rejected. Either way, signing in again yields tokens
// issued after the logout, which are unaffected.

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

const (
	// backchannelLogoutEvent is the "events" member every logout token
	// must carry (Back-Channel Logout 1.0, section 2.4).
	backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

	// ssoLogoutRetention is how long an accepted logout keeps rejecting
	// access tokens. It must outlast the provider's access token lifetime:
	// a token issued just before the logout and still unexpired after this
	// would be accepted again.
	ssoLogoutRetention = 24 * time.Hour

	// maxLogoutRequestBytes bounds a back-channel logout request body, which
	// carries nothing but one form-encoded JWT.
	maxLogoutRequestBytes = 64 << 10
)

// errSSOSessionEnded is returned by ssoBearerUser for an access token whose
// session the provider has since ended via back-channel logout.
var errSSOSessionEnded = errors.New("sso session was ended by a back-channel logout")

// ssoSession identifies the provider session an SSO access token belongs
// to, as far as back-channel logout needs it: its subject, its "sid" claim
// (empty if the provider doesn't put one in access tokens) and when it was
// issued. The zero value (an API token's principal) belongs to no session.
type ssoSession struct {
	subject   string
	sessionID string
	issuedAt  time.Time
}

// subjectLogout is what ssoLogouts remembers per subject.
type subjectLogout struct {
	// all is the latest logout of all the subject's sessions (a logout
	// token without a sid): any of their access tokens issued at or before
	// it is rejected.
	all time.Time

	// unscoped is the latest logout of one of the subject's sessions: their
	// access tokens without a sid issued at or before it are rejected.
	unscoped time.Time

	// expires is when the latest of those logouts stops applying.
	expires time.Time
}

// ssoLogouts holds the back-channel logouts still in effect for one issuer:
// accepted by handleBackchannelLogout, persisted in db (nil keeps them in
// memory only), and consulted by ssoBearerUser on every request. Safe for
// concurrent use.
type ssoLogouts struct {
	issuer string
	db     *store.Store

	mu       sync.RWMutex
	sessions map[string]time.Time // sid -> when its logout stops applying
	subjects map[string]subjectLogout

	// changed is closed (and replaced) on every recorded logout, waking
	// whoever watches a session (see untilEnded).
	changed chan struct{}
}

// newSSOLogouts returns the logouts in effect for issuer, loaded from db.
// A failure to load them is returned alongside a usable, empty set, so the
// caller can decide whether to carry on without them.
func newSSOLogouts(ctx context.Context, issuer string, db *store.Store) (*ssoLogouts, error) {
	l := &ssoLogouts{
		issuer:   issuer,
		db:       db,
		sessions: map[string]time.Time{},
		subjects: map[string]subjectLogout{},
		changed:  make(chan struct{}),
	}

	if db == nil || issuer == "" {
		return l, nil
	}

	now := time.Now()

	if err := db.DeleteExpiredSSOLogouts(ctx, now); err != nil {
		return l, err
	}

	saved, err := db.ListSSOLogouts(ctx, issuer, now)
	if err != nil {
		return l, err
	}

	for _, s := range saved {
		l.apply(s)
	}

	return l, nil
}

// record makes logout take effect immediately, then persists it. A
// persisting error is returned, but the logout still applies until this
// process exits.
func (l *ssoLogouts) record(ctx context.Context, logout store.SSOLogout) error {
	l.mu.Lock()
	l.prune(time.Now())
	l.apply(logout)
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()

	if l.db == nil {
		return nil
	}

	if err := l.db.SaveSSOLogout(ctx, logout); err != nil {
		return err
	}

	return l.db.DeleteExpiredSSOLogouts(ctx, time.Now())
}

// apply adds logout to the in-memory maps. The caller must hold mu for
// writing, or be the only one with access to l.
func (l *ssoLogouts) apply(logout store.SSOLogout) {
	if logout.SessionID != "" {
		l.sessions[logout.SessionID] = laterOf(l.sessions[logout.SessionID], logout.ExpiresAt)
	}

	if logout.Subject == "" {
		return
	}

	s := l.subjects[logout.Subject]
	if logout.SessionID == "" {
		s.all = laterOf(s.all, logout.LoggedOutAt)
	} else {
		s.unscoped = laterOf(s.unscoped, logout.LoggedOutAt)
	}

	s.expires = laterOf(s.expires, logout.ExpiresAt)
	l.subjects[logout.Subject] = s
}

// prune forgets every logout no longer in effect at now. The caller must
// hold mu for writing.
func (l *ssoLogouts) prune(now time.Time) {
	for sid, expires := range l.sessions {
		if !expires.After(now) {
			delete(l.sessions, sid)
		}
	}

	for sub, s := range l.subjects {
		if !s.expires.After(now) {
			delete(l.subjects, sub)
		}
	}
}

// ended reports whether a recorded logout covers session s (see the file
// comment for which tokens a logout covers).
func (l *ssoLogouts) ended(s ssoSession) bool {
	if s == (ssoSession{}) {
		return false
	}

	now := time.Now()

	l.mu.RLock()
	defer l.mu.RUnlock()

	if expires, ok := l.sessions[s.sessionID]; ok && s.sessionID != "" && expires.After(now) {
		return true
	}

	sub, ok := l.subjects[s.subject]
	if !ok || s.subject == "" || !sub.expires.After(now) {
		return false
	}

	if !sub.all.IsZero() && !s.issuedAt.After(sub.all) {
		return true
	}

	return s.sessionID == "" && !sub.unscoped.IsZero() && !s.issuedAt.After(sub.unscoped)
}

// changes returns a channel closed by the next recorded logout.
func (l *ssoLogouts) changes() <-chan struct{} {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.changed
}

// untilEnded returns a copy of ctx that is also cancelled, with cause
// errSSOSessionEnded, once a logout covering s is recorded. A zero s (no SSO
// session, e.g. an API token) returns ctx itself.
func (l *ssoLogouts) untilEnded(ctx context.Context, s ssoSession) context.Context {
	if s == (ssoSession{}) {
		return ctx
	}

	ctx, cancel := context.WithCancelCause(ctx)

	go func() {
		for {
			changed := l.changes()
			if l.ended(s) {
				cancel(errSSOSessionEnded)
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
		}
	}()

	return ctx
}

// laterOf returns the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// logoutTokenClaims are the logout-token claims verifyLogoutToken checks
// beyond what go-oidc already verifies (signature, issuer, expiry).
type logoutTokenClaims struct {
	SessionID string                     `json:"sid"`
	Events    map[string]json.RawMessage `json:"events"`
	Nonce     json.RawMessage            `json:"nonce"`
}

// verifyLogoutToken validates rawToken as a logout token from s's provider
// (Back-Channel Logout 1.0, section 2.6) and returns the logout it asks for,
// taking effect at the token's iat and applying until now plus
// ssoLogoutRetention. Its jti isn't checked for replay: recording the same
// logout twice changes nothing.
func verifyLogoutToken(ctx context.Context, s config.OIDCSettings, cache *ssoVerifierCache, rawToken string, now time.Time) (store.SSOLogout, error) {
	verifier, err := cache.get(ctx, s.Issuer)
	if err != nil {
		return store.SSOLogout{}, err
	}

	token, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return store.SSOLogout{}, fmt.Errorf("verify logout token: %w", err)
	}

	var claims logoutTokenClaims
	if err := token.Claims(&claims); err != nil {
		return store.SSOLogout{}, fmt.Errorf("decode logout token claims: %w", err)
	}

	// Unlike an access token (see tokenIssuedTo), a logout token's audience
	// is specified: it must name the client.
	if !slices.Contains(token.Audience, s.ClientID) {
		return store.SSOLogout{}, errors.New("logout token was not issued to the configured client")
	}

	// The events claim and the absent nonce are what tell a logout token
	// apart from an ID token signed with the same key.
	if _, ok := claims.Events[backchannelLogoutEvent]; !ok {
		return store.SSOLogout{}, errors.New("logout token has no back-channel logout event")
	}

	if len(claims.Nonce) > 0 {
		return store.SSOLogout{}, errors.New("logout token must not contain a nonce")
	}

	if token.IssuedAt.IsZero() {
		return store.SSOLogout{}, errors.New("logout token has no iat")
	}

	if token.Subject == "" && claims.SessionID == "" {
		return store.SSOLogout{}, errors.New("logout token has neither sub nor sid")
	}

	return store.SSOLogout{
		Issuer:      s.Issuer,
		Subject:     token.Subject,
		SessionID:   claims.SessionID,
		LoggedOutAt: token.IssuedAt,
		ExpiresAt:   now.Add(ssoLogoutRetention),
	}, nil
}

// handleBackchannelLogout serves POST /api/sso/backchannel-logout, the
// provider's back-channel logout endpoint (register
// <origin>/api/sso/backchannel-logout as the client's backchannel logout
// URL). It isn't gated behind requireUser: the provider calls it directly,
// authenticated only by the logout token's signature. It answers 200 once
// the logout applies, or 400 with an OAuth-style error body (Back-Channel
// Logout 1.0, section 2.8) for a missing or invalid logout token. With SSO
// disabled it doesn't exist (404).
func handleBackchannelLogout(a *authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		if !a.oidc.Enabled {
			http.NotFound(w, r)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxLogoutRequestBytes)
		if err := r.ParseForm(); err != nil {
			a.log.Warn("sso: back-channel logout rejected", "err", err)
			writeLogoutError(w)

			return
		}

		logout, err := verifyLogoutToken(r.Context(), a.oidc, a.verifiers, r.PostForm.Get("logout_token"), time.Now())
		if err != nil {
			a.log.Warn("sso: back-channel logout rejected", "err", err)
			writeLogoutError(w)

			return
		}

		if err := a.logouts.record(r.Context(), logout); err != nil {
			a.log.Warn("sso: storing back-channel logout failed, it only applies until restart", "err", err)
		}

		a.log.Info("sso: back-channel logout", "subject", logout.Subject, "session_scoped", logout.SessionID != "")
		w.WriteHeader(http.StatusOK)
	}
}

// writeLogoutError answers a rejected back-channel logout request. The
// specific reason is only logged server-side.
func writeLogoutError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
}

// newAuthenticatorLogouts loads the back-channel logouts for s's issuer,
// carrying on with an empty set if the state db can't provide them.
func newAuthenticatorLogouts(ctx context.Context, s config.OIDCSettings, db *store.Store, log *slog.Logger) *ssoLogouts {
	var issuer string
	if s.Enabled {
		issuer = s.Issuer
	}

	logouts, err := newSSOLogouts(ctx, issuer, db)
	if err != nil {
		log.Warn("web UI: loading sso back-channel logouts failed, earlier logouts are not enforced", "err", err)
	}

	return logouts
}
