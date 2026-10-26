package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// TestRecentLoginRequiredForCommands checks that creating a command needs
// an ID token showing a sign-in within ReauthMaxAge, for the same user,
// while other changes (running a job here) don't.
func TestRecentLoginRequiredForCommands(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	oidc := idp.settings()
	oidc.ReauthMaxAge = 10 * time.Minute
	srv, _ := startJobsWebUIWithOIDC(t, oidc, true, nil)

	admin := idp.token(t, "erin", map[string]any{"groups": []string{"admins"}, "sid": "s1"})
	idToken := func(subject, sid string, authTime time.Time) string {
		return idp.token(t, subject, map[string]any{"aud": testClientID, "sid": sid, "auth_time": authTime.Unix()})
	}

	now := time.Now()

	for _, c := range []struct {
		name, id string
		want     int
	}{
		{"no ID token", "", http.StatusUnauthorized},
		{"stale sign-in", idToken("erin", "s1", now.Add(-time.Hour)), http.StatusUnauthorized},
		{"another user's", idToken("mallory", "s1", now), http.StatusUnauthorized},
		{"another session's", idToken("erin", "s2", now), http.StatusUnauthorized},
		{"without auth_time", idp.token(t, "erin", map[string]any{"aud": testClientID}), http.StatusUnauthorized},
		{"recent sign-in", idToken("erin", "s1", now.Add(-time.Minute)), http.StatusCreated},
	} {
		code, header := postWithIDToken(t, srv, "/api/command-configs", admin, c.id, config.FileCommand{ID: "page", Cmd: "echo page"})
		if code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, code, c.want)
		}

		if c.want == http.StatusUnauthorized && !strings.Contains(header.Get("WWW-Authenticate"), `error="insufficient_user_authentication"`) {
			t.Errorf("%s: WWW-Authenticate = %q, want an insufficient_user_authentication challenge", c.name, header.Get("WWW-Authenticate"))
		}
	}

	if code, _ := postWithIDToken(t, srv, "/api/jobs/missing/run", admin, "", nil); code == http.StatusUnauthorized {
		t.Errorf("job run = 401, want it not to need a recent sign-in")
	}
}

// postWithIDToken POSTs body as JSON to path with token as the bearer token
// and idToken (if any) in idTokenHeader, returning the response's status and
// headers.
func postWithIDToken(t *testing.T, srv *Server, path, token, idToken string, body any) (int, http.Header) {
	t.Helper()

	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+srv.addr+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	if idToken != "" {
		req.Header.Set(idTokenHeader, idToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode, resp.Header
}
