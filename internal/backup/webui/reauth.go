package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// idTokenHeader carries the SPA's current ID token on a request that
// needs a recent sign-in (see requireRecentLogin): its auth_time is when
// the user last actually authenticated at the provider, which the access
// token doesn't reliably say.
const idTokenHeader = "X-ID-Token"

// errNoIDToken is returned by recentLoginTime when the request carries no
// ID token.
var errNoIDToken = errors.New("no ID token sent")

// requireRecentLogin wraps next — a change that could run code on this
// host or redirect backups (jobs, servers, commands, GPG keys) — so it's
// only served when the signed-in user authenticated at the provider within
// a.oidc.ReauthMaxAge, as the ID token in idTokenHeader shows. Otherwise
// it answers 401 with an RFC 9470 insufficient_user_authentication
// challenge carrying max_age, on which the SPA has the user sign in again.
// A zero ReauthMaxAge (or SSO off, where nobody gets this far) serves next
// as is. It must run behind requireUser.
func requireRecentLogin(a *authenticator, next http.HandlerFunc) http.HandlerFunc {
	maxAge := a.oidc.ReauthMaxAge
	if !a.oidc.Enabled || maxAge <= 0 {
		return next
	}

	return func(w http.ResponseWriter, r *http.Request) {
		user, _ := currentUser(r.Context())

		authTime, err := recentLoginTime(r.Context(), a, r.Header.Get(idTokenHeader), user)
		if err == nil && time.Since(authTime) > maxAge {
			err = fmt.Errorf("last sign-in at %s is older than %s", authTime.UTC().Format(time.RFC3339), maxAge)
		}

		if err != nil {
			a.log.Info("web UI: change needs a recent sign-in", "by", user.Username, "path", r.URL.Path, "reason", err)

			seconds := int(maxAge / time.Second)
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer error="insufficient_user_authentication", error_description="a more recent sign-in is required", max_age=%d`, seconds))
			http.Error(w, fmt.Sprintf("this change needs a sign-in within the last %s: sign in again, then repeat it", maxAge), http.StatusUnauthorized)

			return
		}

		next(w, r)
	}
}

// recentLoginTime verifies rawIDToken as an ID token the provider issued
// to the configured client for user (same subject and, when both name one,
// the same provider session) and returns its auth_time.
func recentLoginTime(ctx context.Context, a *authenticator, rawIDToken string, user *principal) (time.Time, error) {
	rawIDToken = strings.TrimSpace(rawIDToken)
	if rawIDToken == "" {
		return time.Time{}, errNoIDToken
	}

	if user == nil || user.session.subject == "" {
		return time.Time{}, errors.New("not signed in via SSO")
	}

	verifier, err := a.verifiers.get(ctx, a.oidc.Issuer)
	if err != nil {
		return time.Time{}, err
	}

	token, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return time.Time{}, fmt.Errorf("verify ID token: %w", err)
	}

	// Unlike an access token's (see tokenIssuedTo), an ID token's audience
	// is specified: it must name the client.
	if !slices.Contains(token.Audience, a.oidc.ClientID) {
		return time.Time{}, errors.New("ID token was not issued to the configured client")
	}

	var claims struct {
		AuthTime  int64  `json:"auth_time"`
		SessionID string `json:"sid"`
	}
	if err := token.Claims(&claims); err != nil {
		return time.Time{}, fmt.Errorf("decode ID token claims: %w", err)
	}

	switch {
	case token.Subject != user.session.subject:
		return time.Time{}, errors.New("ID token belongs to another user")
	case claims.SessionID != "" && user.session.sessionID != "" && claims.SessionID != user.session.sessionID:
		return time.Time{}, errors.New("ID token belongs to another session")
	case claims.AuthTime <= 0:
		return time.Time{}, errors.New("ID token has no auth_time")
	}

	return time.Unix(claims.AuthTime, 0), nil
}
