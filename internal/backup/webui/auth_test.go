package webui

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

const testClientID = "backup-ui"

// testIDP is a minimal OpenID Connect provider for tests: just discovery
// and a JWKS, plus a signer minting access tokens with its key.
type testIDP struct {
	srv    *httptest.Server
	signer jose.Signer
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building signer: %v", err)
	}

	idp := &testIDP{signer: signer}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 idp.srv.URL,
			"authorization_endpoint": idp.srv.URL + "/auth",
			"token_endpoint":         idp.srv.URL + "/token",
			"jwks_uri":               idp.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})

	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)

	return idp
}

// token mints an access token issued to testClientID (as azp) for subject,
// valid for an hour unless extra overrides "exp", with extra merged into
// its claims.
func (idp *testIDP) token(t *testing.T, subject string, extra map[string]any) string {
	t.Helper()

	now := time.Now()
	claims := map[string]any{
		"iss": idp.srv.URL,
		"sub": subject,
		"aud": "account",
		"azp": testClientID,
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}

	maps.Copy(claims, extra)

	raw, err := jwt.Signed(idp.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}

	return raw
}

// settings returns enabled OIDCSettings pointing at idp, granting "view"
// by default, "admin" to the "admins" group, and "login-log" to "auditors".
func (idp *testIDP) settings() config.OIDCSettings {
	return config.OIDCSettings{
		Enabled:            true,
		Issuer:             idp.srv.URL,
		ClientID:           testClientID,
		Scopes:             []string{"openid", "profile", "offline_access"},
		ButtonLabel:        "Sign in with SSO",
		GroupsClaim:        "groups",
		DefaultPermissions: permission.PermissionView,
		GroupPermissions: map[string]permission.Permission{
			"admins":   permission.PermissionAdmin,
			"auditors": permission.PermissionViewLoginLog,
		},
	}
}

func TestGroupsFromClaim(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{``, nil},
		{`["a","b"]`, []string{"a", "b"}},
		{`"solo"`, []string{"solo"}},
		{`""`, nil},
		{`42`, nil},
		{`{"a":1}`, nil},
	} {
		if got := groupsFromClaim(json.RawMessage(tc.raw)); !slices.Equal(got, tc.want) {
			t.Errorf("groupsFromClaim(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestGroupGrantsUnion(t *testing.T) {
	t.Parallel()

	mapping := map[string]permission.Permission{
		"ops":      permission.PermissionView | permission.PermissionDownload,
		"auditors": permission.PermissionViewLoginLog,
	}

	got := groupGrants(mapping, []string{"ops", "auditors", "unmapped"})
	if want := permission.PermissionView | permission.PermissionDownload | permission.PermissionViewLoginLog; got != want {
		t.Errorf("groupGrants() = %v, want %v", got, want)
	}

	if none := groupGrants(mapping, []string{"unmapped"}); none != 0 {
		t.Errorf("unmapped group granted %v", none)
	}
}

func TestTokenIssuedTo(t *testing.T) {
	t.Parallel()

	if !tokenIssuedTo("c", "c", nil) {
		t.Error("matching azp rejected")
	}

	if !tokenIssuedTo("c", "", []string{"account", "c"}) {
		t.Error("matching aud rejected")
	}

	if tokenIssuedTo("c", "other", []string{"account"}) {
		t.Error("token for another client accepted")
	}
}

func TestUsernameFromClaims(t *testing.T) {
	t.Parallel()

	if got := usernameFromClaims("a@x", "alice", "sub"); got != "alice" {
		t.Errorf("got %q, want preferred_username", got)
	}

	if got := usernameFromClaims("a@x", "", "sub"); got != "a@x" {
		t.Errorf("got %q, want email", got)
	}

	if got := usernameFromClaims("", "", "sub"); got != "sub" {
		t.Errorf("got %q, want subject", got)
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	for header, want := range map[string]string{
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"Basic abc":    "",
		"Bearer ":      "",
		"":             "",
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.Header.Set("Authorization", header)

		got, ok := bearerToken(r)
		if got != want || ok != (want != "") {
			t.Errorf("bearerToken(%q) = (%q, %v), want %q", header, got, ok, want)
		}
	}
}

func TestSSOBearerUser(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	s := idp.settings()
	cache := newSSOVerifierCache()

	user, err := ssoBearerUser(t.Context(), s, cache, idp.token(t, "u1", map[string]any{
		"preferred_username": "alice", "groups": []string{"auditors", "unmapped"},
	}))
	if err != nil {
		t.Fatalf("ssoBearerUser() error: %v", err)
	}

	if want := (principal{Username: "alice", Perm: permission.PermissionView | permission.PermissionViewLoginLog}); *user != want {
		t.Errorf("principal = %+v, want %+v", *user, want)
	}

	// A single-string groups claim, read from a non-default claim name.
	s.GroupsClaim = "roles"

	user, err = ssoBearerUser(t.Context(), s, cache, idp.token(t, "u2", map[string]any{"roles": "admins"}))
	if err != nil || !user.Perm.CanAdmin() || user.Username != "u2" {
		t.Errorf("ssoBearerUser(roles=admins) = (%+v, %v), want admin principal u2", user, err)
	}

	if _, err := ssoBearerUser(t.Context(), s, cache, idp.token(t, "u3", map[string]any{"azp": "other-client"})); err == nil {
		t.Error("token issued to another client accepted")
	}

	_, err = ssoBearerUser(t.Context(), s, cache, idp.token(t, "u4", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}))
	if !isTokenExpired(err) {
		t.Errorf("expired token error = %v, want a TokenExpiredError", err)
	}

	if _, err := ssoBearerUser(t.Context(), config.OIDCSettings{}, cache, "x"); !errors.Is(err, errSSODisabled) {
		t.Errorf("disabled SSO error = %v, want errSSODisabled", err)
	}
}

// startSSOWebUI starts a web UI wired to idp's settings, with a state db.
func startSSOWebUI(t *testing.T, idp *testIDP, receivers map[string]config.ResolvedReceiver) *Server {
	t.Helper()

	store, _ := newTestStore()
	db := openTestStateDB(t)

	srv := StartWebUI("127.0.0.1:0", store, nil, nil, receivers, nil, discardLogger, db, nil, idp.settings(), nil, false, false, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv
}

// TestStartWebUIPermissionGates is an end-to-end check, through the real
// mux StartWebUI wires up, that every gate reads its permission from the
// SSO token: default-permissions alone grants "view", and each group adds
// only what it's mapped to.
func TestStartWebUIPermissionGates(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv := startSSOWebUI(t, idp, nil)
	client := &http.Client{}

	viewer := idp.token(t, "viewer", nil)
	auditor := idp.token(t, "auditor", map[string]any{"groups": []string{"auditors"}})
	admin := idp.token(t, "admin", map[string]any{"groups": []string{"admins"}})

	tests := []struct {
		name  string
		token string
		path  string
		want  int
	}{
		{"no token", "", "/api/status", http.StatusUnauthorized},
		{"garbage token", "not-a-jwt", "/api/status", http.StatusUnauthorized},
		{"viewer can see status", viewer, "/api/status", http.StatusOK},
		{"viewer cannot see login log", viewer, "/api/login-events", http.StatusForbidden},
		{"viewer cannot see download log", viewer, "/api/download-events", http.StatusForbidden},
		{"viewer cannot see job run log", viewer, "/api/job-runs", http.StatusForbidden},
		{"auditor can see login log", auditor, "/api/login-events", http.StatusOK},
		{"auditor cannot see receiver log", auditor, "/api/receiver-events", http.StatusForbidden},
		{"admin can see job run log", admin, "/api/job-runs", http.StatusOK},
		{"admin can see target run log", admin, "/api/target-runs", http.StatusOK},
		{"admin can see receiver log", admin, "/api/receiver-events", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := webUIGetStatus(t, client, srv, tt.token, tt.path); got != tt.want {
				t.Errorf("GET %s status = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}

// doJSON issues method path on srv with token, returning the status code
// and decoding a 200 body into out (if non-nil).
func doJSON(t *testing.T, srv *Server, method, path, token string, out any) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+srv.addr+path, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
	}

	return resp.StatusCode
}

func TestSSOStatusAndMe(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv := startSSOWebUI(t, idp, nil)

	var status ssoStatusJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/sso/status", "", &status); code != http.StatusOK {
		t.Fatalf("GET /api/sso/status = %d", code)
	}

	want := ssoStatusJSON{Enabled: true, ButtonLabel: "Sign in with SSO", IssuerURL: idp.srv.URL, ClientID: testClientID, Scopes: "openid profile offline_access"}
	if status != want {
		t.Errorf("sso status = %+v, want %+v", status, want)
	}

	token := idp.token(t, "u1", map[string]any{"email": "bob@example.com", "groups": []string{"admins"}})

	var me meJSON

	if code := doJSON(t, srv, http.MethodGet, "/api/me", token, &me); code != http.StatusOK {
		t.Fatalf("GET /api/me = %d", code)
	}

	if me.Username != "bob@example.com" || !me.Admin || !slices.Equal(me.Permissions, []string{"view", "admin"}) {
		t.Errorf("me = %+v, want bob@example.com with view+admin", me)
	}
}

func TestSSOLoginRecordsLoginEvents(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	store, _ := newTestStore()
	db := openTestStateDB(t)

	srv := StartWebUI("127.0.0.1:0", store, nil, nil, nil, nil, discardLogger, db, nil, idp.settings(), nil, false, false, "", nil, nil)
	if srv == nil {
		t.Fatal("StartWebUI() = nil")
	}

	t.Cleanup(srv.Shutdown)

	token := idp.token(t, "u1", map[string]any{"preferred_username": "carol"})

	// GET /api/me is called on every page load and must log nothing; only
	// POST /api/sso/login records the sign-in.
	doJSON(t, srv, http.MethodGet, "/api/me", token, nil)

	var me meJSON
	if code := doJSON(t, srv, http.MethodPost, "/api/sso/login", token, &me); code != http.StatusOK || me.Username != "carol" {
		t.Fatalf("POST /api/sso/login = (%d, %+v), want 200 for carol", code, me)
	}

	// An expired token is routine (an idle tab) and isn't logged; a token
	// for another client is.
	expired := idp.token(t, "u1", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
	if code := doJSON(t, srv, http.MethodGet, "/api/status", expired, nil); code != http.StatusUnauthorized {
		t.Errorf("expired token = %d, want 401", code)
	}

	wrongClient := idp.token(t, "u1", map[string]any{"azp": "other"})
	if code := doJSON(t, srv, http.MethodGet, "/api/status", wrongClient, nil); code != http.StatusUnauthorized {
		t.Errorf("wrong-client token = %d, want 401", code)
	}

	events, err := db.ListLoginEvents(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListLoginEvents() error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("ListLoginEvents() = %+v, want exactly 2 events", events)
	}

	if events[0].Success || !strings.Contains(events[0].Detail, "configured client") {
		t.Errorf("events[0] = %+v, want the failed wrong-client attempt", events[0])
	}

	if !events[1].Success || events[1].Username != "carol" || events[1].Method != loginMethodSSO {
		t.Errorf("events[1] = %+v, want carol's successful SSO login", events[1])
	}
}

// TestDownloadTicketAttributedToSSOUser checks the mint-then-navigate
// download flow end to end: minting needs a bearer token with "download",
// and the ticket carries that user's name into the download log.
func TestDownloadTicketAttributedToSSOUser(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "backup.gpg"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	idp := newTestIDP(t)
	srv := startSSOWebUI(t, idp, map[string]config.ResolvedReceiver{"a": {ID: "a", Path: root}})

	viewer := idp.token(t, "viewer", nil)
	if code := doJSON(t, srv, http.MethodPost, "/api/receivers/a/download/backup.gpg", viewer, nil); code != http.StatusForbidden {
		t.Errorf("mint without download permission = %d, want 403", code)
	}

	admin := idp.token(t, "dave", map[string]any{"groups": []string{"admins"}})

	var ticket ticketJSON
	if code := doJSON(t, srv, http.MethodPost, "/api/receivers/a/download/backup.gpg", admin, &ticket); code != http.StatusOK || ticket.Ticket == "" {
		t.Fatalf("mint = (%d, %+v), want a ticket", code, ticket)
	}

	if code := doJSON(t, srv, http.MethodGet, "/api/receivers/a/download/backup.gpg?ticket="+ticket.Ticket, "", nil); code != http.StatusOK {
		t.Fatalf("download with ticket = %d, want 200", code)
	}

	var events []downloadEventJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/download-events", admin, &events); code != http.StatusOK {
		t.Fatalf("GET /api/download-events = %d", code)
	}

	if len(events) != 1 || events[0].Username != "dave" || !events[0].Success {
		t.Errorf("download events = %+v, want one successful download by dave", events)
	}
}
