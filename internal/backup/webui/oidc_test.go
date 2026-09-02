package webui

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	josejwk "github.com/go-jose/go-jose/v4"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// fakeOIDCProvider is a minimal OpenID Connect provider backed by an
// httptest.Server: enough of discovery (/.well-known/openid-configuration),
// JWKS (/jwks), and token exchange (/token) for newOIDCAuth/handleOIDCLogin/
// handleOIDCCallback to be exercised against a real (if fake) provider,
// rather than mocking those functions' internals directly.
type fakeOIDCProvider struct {
	srv        *httptest.Server
	privateKey *rsa.PrivateKey
	clientID   string

	// nonce is read by the /token handler when signing the ID token it
	// returns, so a test can point it at whatever nonce handleOIDCLogin
	// actually generated for the login under test (see newFakeOIDCProvider
	// and TestOIDCLoginAndCallback).
	nonce atomic.Value // string

	// email, if non-empty, is embedded as the ID token's "email" claim.
	email atomic.Value // string
}

func newFakeOIDCProvider(t *testing.T, clientID string) *fakeOIDCProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	p := &fakeOIDCProvider{privateKey: key, clientID: clientID}
	p.nonce.Store("")
	p.email.Store("")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("GET /jwks", p.handleJWKS)
	mux.HandleFunc("POST /token", p.handleToken)

	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)

	return p
}

func (p *fakeOIDCProvider) issuer() string { return p.srv.URL }

func (p *fakeOIDCProvider) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                p.issuer(),
		"authorization_endpoint":                p.issuer() + "/auth",
		"token_endpoint":                        p.issuer() + "/token",
		"jwks_uri":                              p.issuer() + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *fakeOIDCProvider) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	set := josejwk.JSONWebKeySet{
		Keys: []josejwk.JSONWebKey{
			{Key: &p.privateKey.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}

func (p *fakeOIDCProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	idToken, err := p.signIDToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

// signIDToken builds and RS256-signs a minimal ID token JWT for p's client,
// embedding whatever nonce/email the test has set (see fakeOIDCProvider's
// nonce/email fields).
func (p *fakeOIDCProvider) signIDToken() (string, error) {
	now := time.Now()

	nonce, _ := p.nonce.Load().(string)

	claims := map[string]any{
		"iss":   p.issuer(),
		"aud":   p.clientID,
		"sub":   "user-123",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": nonce,
	}

	if email, _ := p.email.Load().(string); email != "" {
		claims["email"] = email
	}

	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)

	sig, err := signRS256(p.privateKey, signingInput)
	if err != nil {
		return "", err
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func signRS256(key *rsa.PrivateKey, signingInput string) ([]byte, error) {
	sum := sha256.Sum256([]byte(signingInput))
	return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
}

func TestOIDCLoginAndCallback(t *testing.T) {
	t.Parallel()

	const clientID = "test-client"

	provider := newFakeOIDCProvider(t, clientID)
	provider.email.Store("person@example.com")

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:      true,
		Issuer:       provider.issuer(),
		ClientID:     clientID,
		ClientSecret: "test-secret",
		RedirectURL:  "https://backups.example.com/login/oidc/callback",
		Scopes:       []string{"profile", "email"},
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	// Step 1: GET /login/oidc redirects to the provider, carrying a fresh
	// state and nonce.
	loginReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login/oidc?next=/dashboard", nil)
	loginRec := httptest.NewRecorder()

	handleOIDCLogin(auth, pending)(loginRec, loginReq)

	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("GET /login/oidc status = %d, want %d", loginRec.Code, http.StatusSeeOther)
	}

	authURL, err := url.Parse(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing redirect Location: %v", err)
	}

	state := authURL.Query().Get("state")
	nonce := authURL.Query().Get("nonce")

	if state == "" || nonce == "" {
		t.Fatalf("redirect Location = %q, want state and nonce query params", authURL)
	}

	// Step 2: the provider "authenticates" the user and issues an ID token
	// carrying that same nonce (see fakeOIDCProvider.handleToken).
	provider.nonce.Store(nonce)

	// Step 3: GET /login/oidc/callback, as the browser would arrive after
	// the provider's own login, exchanges the code and starts a session.
	callbackReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("/login/oidc/callback?state=%s&code=test-code", url.QueryEscape(state)), nil)
	callbackReq.RemoteAddr = "203.0.113.1:12345"
	callbackRec := httptest.NewRecorder()

	db := openTestStateDB(t)

	handleOIDCCallback(auth, pending, sessions, discardLogger, db, false)(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d; body = %s", callbackRec.Code, http.StatusOK, callbackRec.Body.String())
	}

	body := callbackRec.Body.String()

	// html/template JS-escapes "/" as "\/" inside a <script> string literal,
	// so this looks for the escaped form rather than a literal "/dashboard".
	if !strings.Contains(body, `location.replace("\/dashboard")`) {
		t.Errorf("callback body doesn't send the browser on to /dashboard: %s", body)
	}

	assertSoleLoginEvent(t, db, store.LoginEvent{Username: "person@example.com", Method: "oidc", Success: true})

	token := extractOIDCCompleteToken(t, body)
	if !sessions.valid(token) {
		t.Error("the embedded token isn't a valid session")
	}
}

// TestOIDCCallbackUsesStoredPermissionOverride checks that a permission
// override stored for an identity (see store.GetOrProvisionOIDCUser/
// users.go — set through the "Users" admin section, see handleUpdateUser
// in webui.go) takes precedence over auth.defaultPerm at that identity's
// next SSO login (see handleOIDCCallback), the way TestOIDCLoginAndCallback's
// own session (person@example.com, no override) gets auth.defaultPerm
// instead.
func TestOIDCCallbackUsesStoredPermissionOverride(t *testing.T) {
	t.Parallel()

	const clientID = "test-client"

	provider := newFakeOIDCProvider(t, clientID)
	provider.email.Store("override@example.com")

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:            true,
		Issuer:             provider.issuer(),
		ClientID:           clientID,
		ClientSecret:       "test-secret",
		RedirectURL:        "https://backups.example.com/login/oidc/callback",
		Scopes:             []string{"profile", "email"},
		DefaultPermissions: permission.PermissionView | permission.PermissionDownload,
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	db := openTestStateDB(t)
	if _, err := db.GetOrProvisionOIDCUser(t.Context(), "override@example.com", permission.PermissionView); err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	loginReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login/oidc?next=/dashboard", nil)
	loginRec := httptest.NewRecorder()
	handleOIDCLogin(auth, pending)(loginRec, loginReq)

	authURL, err := url.Parse(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing redirect Location: %v", err)
	}

	state := authURL.Query().Get("state")
	provider.nonce.Store(authURL.Query().Get("nonce"))

	callbackReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("/login/oidc/callback?state=%s&code=test-code", url.QueryEscape(state)), nil)
	callbackRec := httptest.NewRecorder()

	handleOIDCCallback(auth, pending, sessions, discardLogger, db, false)(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d; body = %s", callbackRec.Code, http.StatusOK, callbackRec.Body.String())
	}

	token := extractOIDCCompleteToken(t, callbackRec.Body.String())

	perm := sessions.permissionsFor(&http.Request{Header: http.Header{"Authorization": {"Bearer " + token}}})
	if perm != permission.PermissionView {
		t.Errorf("session permissions = %v, want %v (the stored override, not auth.defaultPerm)", perm, permission.PermissionView)
	}
}

// doOIDCLogin drives one full SSO login (GET /login/oidc, then GET
// /login/oidc/callback) against provider/auth/pending/sessions/db, as a
// browser would, returning the callback's response recorder. Shared by the
// permission-provisioning tests below, which each need to drive more than
// one login in sequence against the same identity.
func doOIDCLogin(t *testing.T, provider *fakeOIDCProvider, auth *OIDCAuth, pending *oidcPendingStore, sessions *sessionStore, db *store.Store) *httptest.ResponseRecorder {
	t.Helper()

	loginReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login/oidc?next=/dashboard", nil)
	loginRec := httptest.NewRecorder()
	handleOIDCLogin(auth, pending)(loginRec, loginReq)

	authURL, err := url.Parse(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing redirect Location: %v", err)
	}

	state := authURL.Query().Get("state")
	provider.nonce.Store(authURL.Query().Get("nonce"))

	callbackReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		fmt.Sprintf("/login/oidc/callback?state=%s&code=test-code", url.QueryEscape(state)), nil)
	callbackRec := httptest.NewRecorder()

	handleOIDCCallback(auth, pending, sessions, discardLogger, db, false)(callbackRec, callbackReq)

	if callbackRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d; body = %s", callbackRec.Code, http.StatusOK, callbackRec.Body.String())
	}

	return callbackRec
}

// TestOIDCCallbackProvisionsPermissionsOnFirstLogin checks that an
// identity's very first SSO login provisions a users row for it (see
// store.GetOrProvisionOIDCUser), granting auth.defaultPerm, so it shows up
// in the "Users" admin section (see handleListUsers in webui.go) ready for
// an admin to adjust, rather than only appearing once they've manually
// added it.
func TestOIDCCallbackProvisionsPermissionsOnFirstLogin(t *testing.T) {
	t.Parallel()

	const clientID = "test-client"

	provider := newFakeOIDCProvider(t, clientID)
	provider.email.Store("newperson@example.com")

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:            true,
		Issuer:             provider.issuer(),
		ClientID:           clientID,
		ClientSecret:       "test-secret",
		RedirectURL:        "https://backups.example.com/login/oidc/callback",
		Scopes:             []string{"profile", "email"},
		DefaultPermissions: permission.PermissionView,
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	db := openTestStateDB(t)
	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	if _, ok, err := db.GetUser(t.Context(), "newperson@example.com"); err != nil || ok {
		t.Fatalf("GetUser() before first login = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	doOIDCLogin(t, provider, auth, pending, sessions, db)

	user, ok, err := db.GetUser(t.Context(), "newperson@example.com")
	if err != nil {
		t.Fatalf("GetUser() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("GetUser() ok = false after first login, want true (a row should have been provisioned)")
	}

	if user.OIDCUsername != "newperson@example.com" {
		t.Errorf("OIDCUsername = %q, want %q", user.OIDCUsername, "newperson@example.com")
	}

	if user.Permissions != permission.PermissionView {
		t.Errorf("provisioned permissions = %v, want %v (auth.defaultPerm)", user.Permissions, permission.PermissionView)
	}
}

// TestOIDCCallbackDoesNotOverwriteAdminEditOnLaterLogin checks that a later
// SSO login for an identity that already has a users row — whether
// auto-provisioned by an earlier login or edited by an admin — doesn't reset
// it back to auth.defaultPerm; only an admin's own edit (see
// handleUpdateUser in webui.go) should change it.
func TestOIDCCallbackDoesNotOverwriteAdminEditOnLaterLogin(t *testing.T) {
	t.Parallel()

	const clientID = "test-client"

	provider := newFakeOIDCProvider(t, clientID)
	provider.email.Store("regular@example.com")

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:            true,
		Issuer:             provider.issuer(),
		ClientID:           clientID,
		ClientSecret:       "test-secret",
		RedirectURL:        "https://backups.example.com/login/oidc/callback",
		Scopes:             []string{"profile", "email"},
		DefaultPermissions: permission.PermissionView | permission.PermissionDownload,
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	db := openTestStateDB(t)
	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	// First login provisions the record with the full default.
	doOIDCLogin(t, provider, auth, pending, sessions, db)

	// An admin then restricts it to view-only.
	if err := db.UpdateUserPermissions(t.Context(), "regular@example.com", permission.PermissionView); err != nil {
		t.Fatalf("UpdateUserPermissions() unexpected error: %v", err)
	}

	// A second login must honor the admin's edit, not reset it back to the
	// configured default.
	rec := doOIDCLogin(t, provider, auth, pending, sessions, db)

	token := extractOIDCCompleteToken(t, rec.Body.String())

	perm := sessions.permissionsFor(&http.Request{Header: http.Header{"Authorization": {"Bearer " + token}}})
	if perm != permission.PermissionView {
		t.Errorf("session permissions after second login = %v, want %v (the admin's edit, not auth.defaultPerm)", perm, permission.PermissionView)
	}
}

// TestOIDCCallbackNeverAdoptsUsernameMatchedRow checks that an SSO login
// never touches a pre-existing password-based user's row just because the
// identity string equals that row's username — an SSO login only ever
// matches by oidc_username (see store.GetOrProvisionOIDCUser), never by
// comparing the identity to an unrelated row's username. Linking the two
// is a deliberate admin action (see handleUpdateUser/store.SetUserOIDCUsername
// in webui.go), never inferred from a login.
func TestOIDCCallbackNeverAdoptsUsernameMatchedRow(t *testing.T) {
	t.Parallel()

	const (
		clientID = "test-client"
		identity = "shared@example.com"
	)

	provider := newFakeOIDCProvider(t, clientID)
	provider.email.Store(identity)

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:            true,
		Issuer:             provider.issuer(),
		ClientID:           clientID,
		ClientSecret:       "test-secret",
		RedirectURL:        "https://backups.example.com/login/oidc/callback",
		Scopes:             []string{"profile", "email"},
		DefaultPermissions: permission.PermissionView,
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	db := openTestStateDB(t)

	// A password-based account already exists, named exactly like the SSO
	// identity that's about to log in.
	if err := db.SaveUser(t.Context(), identity, "hunter2", "", permission.PermissionAdmin); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	doOIDCLogin(t, provider, auth, pending, sessions, db)

	// The pre-existing password account must be untouched: still unlinked,
	// still holding its own permissions.
	existing, ok, err := db.GetUser(t.Context(), identity)
	if err != nil || !ok {
		t.Fatalf("GetUser(%q) after SSO login = (ok=%v, err=%v), want (true, nil)", identity, ok, err)
	}

	if existing.OIDCUsername != "" {
		t.Errorf("pre-existing user's OIDCUsername = %q, want \"\" (untouched by the SSO login)", existing.OIDCUsername)
	}

	if existing.Permissions != permission.PermissionAdmin {
		t.Errorf("pre-existing user's Permissions = %v, want %v (untouched by the SSO login)", existing.Permissions, permission.PermissionAdmin)
	}

	// The SSO login must instead have provisioned its own, distinctly named
	// row, linked via oidc_username to the identity.
	users, err := db.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers() unexpected error: %v", err)
	}

	var linked *store.User

	for i := range users {
		if users[i].OIDCUsername == identity {
			linked = &users[i]
			break
		}
	}

	if linked == nil {
		t.Fatalf("ListUsers() = %+v, want a row linked to %q", users, identity)
	}

	if linked.Username == identity {
		t.Errorf("provisioned row's Username = %q, want a disambiguated name distinct from the pre-existing user", linked.Username)
	}

	if linked.Permissions != permission.PermissionView {
		t.Errorf("provisioned row's Permissions = %v, want %v (auth.defaultPerm)", linked.Permissions, permission.PermissionView)
	}
}

// extractOIDCCompleteToken pulls the bearer token embedded in an
// oidc_complete.html response body (see writeOIDCCompletePage), failing t if
// it's not there.
func extractOIDCCompleteToken(t *testing.T, body string) string {
	t.Helper()

	const prefix = `sessionStorage.setItem("gbt_webui_token", "`

	_, rest, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("body doesn't embed a token: %s", body)
	}

	end := strings.Index(rest, `"`)
	if end == -1 {
		t.Fatalf("body's embedded token has no closing quote: %s", body)
	}

	return rest[:end]
}

// assertSoleLoginEvent fails t unless db's login log holds exactly one
// event matching want's Username/Method/Success (its At/RemoteAddr/Detail
// are ignored, since callers only care about identity/kind/outcome here).
func assertSoleLoginEvent(t *testing.T, db *store.Store, want store.LoginEvent) {
	t.Helper()

	events, err := db.ListLoginEvents(t.Context(), 10)
	if err != nil {
		t.Fatalf("readLoginEvents() error: %v", err)
	}

	if len(events) != 1 || events[0].Username != want.Username || events[0].Method != want.Method || events[0].Success != want.Success {
		t.Errorf("readLoginEvents() = %+v, want one event %+v", events, want)
	}
}

func TestOIDCCallbackUnknownStateRejected(t *testing.T) {
	t.Parallel()

	provider := newFakeOIDCProvider(t, "test-client")

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:      true,
		Issuer:       provider.issuer(),
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURL:  "https://backups.example.com/login/oidc/callback",
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login/oidc/callback?state=bogus&code=test-code", nil)
	rec := httptest.NewRecorder()

	handleOIDCCallback(auth, pending, sessions, discardLogger, nil, false)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestOIDCCallbackProviderErrorRejected(t *testing.T) {
	t.Parallel()

	provider := newFakeOIDCProvider(t, "test-client")

	auth, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:      true,
		Issuer:       provider.issuer(),
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURL:  "https://backups.example.com/login/oidc/callback",
	})
	if err != nil {
		t.Fatalf("newOIDCAuth() unexpected error: %v", err)
	}

	pending := newOIDCPendingStore()
	sessions := newTestSessionStore(t)

	state, _, err := pending.start("/")
	if err != nil {
		t.Fatalf("pending.start(): %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/login/oidc/callback?state="+url.QueryEscape(state)+"&error=access_denied", nil)
	rec := httptest.NewRecorder()

	handleOIDCCallback(auth, pending, sessions, discardLogger, nil, false)(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestOIDCPendingStoreConsumeIsSingleUse(t *testing.T) {
	t.Parallel()

	pending := newOIDCPendingStore()

	state, nonce, err := pending.start("/next")
	if err != nil {
		t.Fatalf("pending.start(): %v", err)
	}

	got, ok := pending.consume(state)
	if !ok || got.nonce != nonce || got.next != "/next" {
		t.Fatalf("consume() = %+v, %v, want nonce %q next %q, true", got, ok, nonce, "/next")
	}

	if _, ok := pending.consume(state); ok {
		t.Error("consume() succeeded a second time for the same state, want single-use")
	}
}

func TestOIDCPendingStoreConsumeExpired(t *testing.T) {
	t.Parallel()

	pending := newOIDCPendingStore()

	state, _, err := pending.start("/")
	if err != nil {
		t.Fatalf("pending.start(): %v", err)
	}

	pending.mu.Lock()
	p := pending.byID[state]
	p.expires = time.Now().Add(-time.Second)
	pending.byID[state] = p
	pending.mu.Unlock()

	if _, ok := pending.consume(state); ok {
		t.Error("consume() succeeded for an expired state, want false")
	}
}

func TestNewOIDCAuthBadIssuerFails(t *testing.T) {
	t.Parallel()

	_, err := newOIDCAuth(t.Context(), config.OIDCSettings{
		Enabled:      true,
		Issuer:       "http://127.0.0.1:1", // nothing listening there
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		RedirectURL:  "https://backups.example.com/login/oidc/callback",
	})
	if err == nil {
		t.Fatal("newOIDCAuth() with an unreachable issuer = nil error, want non-nil")
	}
}

func TestRenderLoginPageShowsSSOLink(t *testing.T) {
	t.Parallel()

	page := renderLoginPage("", "/dashboard", false, true)

	if !strings.Contains(page, `/login/oidc?next=`) {
		t.Error("login page with showSSO=true doesn't link to /login/oidc")
	}

	if strings.Contains(page, `name="username"`) {
		t.Error("login page with showPassword=false still renders a username field")
	}
}

func TestRenderLoginPageShowsBothWithDivider(t *testing.T) {
	t.Parallel()

	page := renderLoginPage("", "/", true, true)

	if !strings.Contains(page, `name="username"`) {
		t.Error("login page with showPassword=true doesn't render a username field")
	}

	if !strings.Contains(page, `/login/oidc?next=`) {
		t.Error("login page with showSSO=true doesn't link to /login/oidc")
	}

	if !strings.Contains(page, `class="divider"`) {
		t.Error("login page with both forms shown doesn't render a divider between them")
	}
}

func TestHandleWebUILoginPasswordPOSTNotFoundWithoutUsername(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionStore(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader("username=a&password=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleWebUILogin("", "", true, sessions, nil, discardLogger, false)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no password auth configured, SSO only)", rec.Code)
	}
}
