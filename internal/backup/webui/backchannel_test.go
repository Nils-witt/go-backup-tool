package webui

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// logoutToken mints a valid logout token from idp for subject, with extra
// merged into (or, set to nil, removed from) its claims.
func (idp *testIDP) logoutToken(t *testing.T, subject string, extra map[string]any) string {
	t.Helper()

	claims := map[string]any{
		"aud":    testClientID,
		"azp":    nil,
		"jti":    "logout-1",
		"events": map[string]any{backchannelLogoutEvent: map[string]any{}},
	}

	maps.Copy(claims, extra)

	return idp.token(t, subject, claims)
}

func TestSSOLogoutsEnded(t *testing.T) {
	t.Parallel()

	now := time.Now()
	before, after := now.Add(-time.Minute), now.Add(time.Minute)
	expires := now.Add(time.Hour)

	l, err := newSSOLogouts(t.Context(), "https://idp", nil)
	if err != nil {
		t.Fatalf("newSSOLogouts() error: %v", err)
	}

	for _, logout := range []store.SSOLogout{
		{Subject: "alice", SessionID: "s1", LoggedOutAt: now, ExpiresAt: expires},
		{Subject: "bob", LoggedOutAt: now, ExpiresAt: expires},
		{SessionID: "s9", LoggedOutAt: now, ExpiresAt: expires},
		{Subject: "dave", LoggedOutAt: now, ExpiresAt: now.Add(-time.Second)},
	} {
		if err := l.record(t.Context(), logout); err != nil {
			t.Fatalf("record() error: %v", err)
		}
	}

	tests := []struct {
		name     string
		session  ssoSession
		expected bool
	}{
		{"no session", ssoSession{}, false},
		{"ended session", ssoSession{subject: "alice", sessionID: "s1", issuedAt: before}, true},
		{"ended session refreshed later", ssoSession{subject: "alice", sessionID: "s1", issuedAt: after}, true},
		{"other session of same subject", ssoSession{subject: "alice", sessionID: "s2", issuedAt: before}, false},
		{"sessionless token issued before", ssoSession{subject: "alice", issuedAt: before}, true},
		{"sessionless token issued after", ssoSession{subject: "alice", issuedAt: after}, false},
		{"all sessions ended, issued before", ssoSession{subject: "bob", sessionID: "s3", issuedAt: before}, true},
		{"all sessions ended, issued after", ssoSession{subject: "bob", sessionID: "s4", issuedAt: after}, false},
		{"session-only logout", ssoSession{subject: "carol", sessionID: "s9", issuedAt: before}, true},
		{"other subject", ssoSession{subject: "erin", issuedAt: before}, false},
		{"expired logout", ssoSession{subject: "dave", issuedAt: before}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := l.ended(tt.session); got != tt.expected {
				t.Errorf("ended(%+v) = %v, want %v", tt.session, got, tt.expected)
			}
		})
	}
}

func TestSSOLogoutsSurviveRestart(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	now := time.Now()

	l, err := newSSOLogouts(t.Context(), "https://idp", db)
	if err != nil {
		t.Fatalf("newSSOLogouts() error: %v", err)
	}

	if err := l.record(t.Context(), store.SSOLogout{Issuer: "https://idp", Subject: "alice", SessionID: "s1", LoggedOutAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("record() error: %v", err)
	}

	reloaded, err := newSSOLogouts(t.Context(), "https://idp", db)
	if err != nil {
		t.Fatalf("newSSOLogouts() after restart error: %v", err)
	}

	if !reloaded.ended(ssoSession{subject: "alice", sessionID: "s1", issuedAt: now.Add(-time.Minute)}) {
		t.Error("logout recorded before restart no longer applies")
	}

	otherIssuer, err := newSSOLogouts(t.Context(), "https://other", db)
	if err != nil {
		t.Fatalf("newSSOLogouts(other issuer) error: %v", err)
	}

	if otherIssuer.ended(ssoSession{subject: "alice", sessionID: "s1", issuedAt: now.Add(-time.Minute)}) {
		t.Error("logout from one issuer applied to another")
	}
}

func TestSSOLogoutsUntilEnded(t *testing.T) {
	t.Parallel()

	l, err := newSSOLogouts(t.Context(), "https://idp", nil)
	if err != nil {
		t.Fatalf("newSSOLogouts() error: %v", err)
	}

	if ctx := l.untilEnded(t.Context(), ssoSession{}); ctx != t.Context() {
		t.Error("untilEnded() wrapped the context of a principal without a session")
	}

	now := time.Now()
	ctx := l.untilEnded(t.Context(), ssoSession{subject: "alice", sessionID: "s1", issuedAt: now.Add(-time.Minute)})

	if err := l.record(t.Context(), store.SSOLogout{Subject: "alice", SessionID: "s2", LoggedOutAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("record() error: %v", err)
	}

	if err := l.record(t.Context(), store.SSOLogout{Subject: "alice", SessionID: "s1", LoggedOutAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("record() error: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context still open 5s after its session was logged out")
	}

	if cause := context.Cause(ctx); !errors.Is(cause, errSSOSessionEnded) {
		t.Errorf("context.Cause() = %v, want errSSOSessionEnded", cause)
	}
}

func TestVerifyLogoutToken(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	s := idp.settings()
	cache := newSSOVerifierCache()
	now := time.Now()

	logout, err := verifyLogoutToken(t.Context(), s, cache, idp.logoutToken(t, "alice", map[string]any{"sid": "s1"}), now)
	if err != nil {
		t.Fatalf("verifyLogoutToken() error: %v", err)
	}

	if logout.Issuer != s.Issuer || logout.Subject != "alice" || logout.SessionID != "s1" || logout.LoggedOutAt.IsZero() || !logout.ExpiresAt.Equal(now.Add(ssoLogoutRetention)) {
		t.Errorf("verifyLogoutToken() = %+v, want alice's session s1 until now+retention", logout)
	}

	tests := []struct {
		name    string
		subject string
		extra   map[string]any
	}{
		{"audience is another client", "alice", map[string]any{"aud": "account", "azp": testClientID}},
		{"no events claim", "alice", map[string]any{"events": nil}},
		{"other event", "alice", map[string]any{"events": map[string]any{"urn:other": map[string]any{}}}},
		{"nonce present", "alice", map[string]any{"nonce": "n"}},
		{"neither sub nor sid", "", nil},
		{"expired", "alice", map[string]any{"exp": now.Add(-time.Minute).Unix()}},
		{"other issuer", "alice", map[string]any{"iss": "https://evil.example"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := verifyLogoutToken(t.Context(), s, cache, idp.logoutToken(t, tt.subject, tt.extra), now); err == nil {
				t.Error("verifyLogoutToken() succeeded, want an error")
			}
		})
	}

	if _, err := verifyLogoutToken(t.Context(), s, cache, "not-a-jwt", now); err == nil {
		t.Error("verifyLogoutToken(garbage) succeeded, want an error")
	}
}

// postLogout POSTs body to srv's back-channel logout endpoint as a form,
// returning the response status.
func postLogout(t *testing.T, srv *Server, form url.Values) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.addr+"/api/sso/backchannel-logout", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/sso/backchannel-logout: %v", err)
	}

	_ = resp.Body.Close()

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	return resp.StatusCode
}

func TestBackchannelLogoutRevokesSession(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv, _, _ := startLiveWebUI(t, idp, false)

	ended := idp.token(t, "alice", map[string]any{"sid": "s1"})
	other := idp.token(t, "alice", map[string]any{"sid": "s2"})

	if code := doJSON(t, srv, http.MethodGet, "/api/me", ended, nil); code != http.StatusOK {
		t.Fatalf("GET /api/me before logout = %d, want 200", code)
	}

	conn, _, err := dialLive(t, srv, mintLiveTicket(t, srv, ended), nil)
	if err != nil {
		t.Fatalf("dialing /api/live: %v", err)
	}

	readLive(t, conn)

	if code := postLogout(t, srv, url.Values{"logout_token": {"not-a-jwt"}}); code != http.StatusBadRequest {
		t.Errorf("invalid logout token = %d, want 400", code)
	}

	if code := postLogout(t, srv, url.Values{}); code != http.StatusBadRequest {
		t.Errorf("missing logout token = %d, want 400", code)
	}

	if code := postLogout(t, srv, url.Values{"logout_token": {idp.logoutToken(t, "alice", map[string]any{"sid": "s1"})}}); code != http.StatusOK {
		t.Fatalf("valid logout token = %d, want 200", code)
	}

	if code := doJSON(t, srv, http.MethodGet, "/api/me", ended, nil); code != http.StatusUnauthorized {
		t.Errorf("GET /api/me with logged-out session = %d, want 401", code)
	}

	if code := doJSON(t, srv, http.MethodGet, "/api/me", other, nil); code != http.StatusOK {
		t.Errorf("GET /api/me with another session = %d, want 200", code)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	for {
		_, _, err = conn.Read(ctx)
		if err != nil {
			break
		}
	}

	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Errorf("live socket close status after logout = %v (err %v), want StatusPolicyViolation", status, err)
	}
}
