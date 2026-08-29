package webui

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
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

	"nilswitt.dev/go-backup-tool/internal/backup"
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

	auth, err := newOIDCAuth(t.Context(), backup.OIDCSettings{
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
	sessions := newSessionStore()

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

	if callbackRec.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want %d; body = %s", callbackRec.Code, http.StatusSeeOther, callbackRec.Body.String())
	}

	if loc := callbackRec.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("callback Location = %q, want %q", loc, "/dashboard")
	}

	cookies := callbackRec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != webUISessionCookie {
		t.Fatalf("callback cookies = %+v, want one %s cookie", cookies, webUISessionCookie)
	}

	assertSoleLoginEvent(t, db, backup.LoginEvent{Username: "person@example.com", Method: "oidc", Success: true})

	if !sessions.valid(cookies[0].Value) {
		t.Error("the session cookie's value isn't a valid session")
	}
}

// assertSoleLoginEvent fails t unless db's login log holds exactly one
// event matching want's Username/Method/Success (its At/RemoteAddr/Detail
// are ignored, since callers only care about identity/kind/outcome here).
func assertSoleLoginEvent(t *testing.T, db *sql.DB, want backup.LoginEvent) {
	t.Helper()

	events, err := backup.ReadLoginEvents(t.Context(), db, 10)
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

	auth, err := newOIDCAuth(t.Context(), backup.OIDCSettings{
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
	sessions := newSessionStore()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/login/oidc/callback?state=bogus&code=test-code", nil)
	rec := httptest.NewRecorder()

	handleOIDCCallback(auth, pending, sessions, discardLogger, nil, false)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	if len(rec.Result().Cookies()) != 0 {
		t.Error("a session cookie was set despite an unknown state")
	}
}

func TestOIDCCallbackProviderErrorRejected(t *testing.T) {
	t.Parallel()

	provider := newFakeOIDCProvider(t, "test-client")

	auth, err := newOIDCAuth(t.Context(), backup.OIDCSettings{
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
	sessions := newSessionStore()

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

	_, err := newOIDCAuth(t.Context(), backup.OIDCSettings{
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

	sessions := newSessionStore()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader("username=a&password=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	handleWebUILogin("", "", true, sessions, nil, discardLogger, false)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no password auth configured, SSO only)", rec.Code)
	}
}
