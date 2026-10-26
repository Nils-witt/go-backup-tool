package receiver

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

func TestRejectedRequestsAreRecorded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	db, err := store.OpenSQLite(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", AllowedServers: []string{testSenderUUID}, TrustedServers: trustSender(&key.PublicKey), Path: filepath.Join(dir, "objects")},
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, NewRegistry(receivers), status.NewReceiverStore(receivers), discardLogger, db, func(*http.Request) string { return "203.0.113.7" })

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Bound to another key than the one it's sent for.
	wrongKey, err := id.SignRequest("instance-a", remoteauth.Request{Method: http.MethodPut, Key: "other.gpg", BodySHA256: "abc"})
	if err != nil {
		t.Fatalf("signing request: %v", err)
	}

	for _, auth := range []string{"", "Bearer not-a-jwt", "Bearer " + wrongKey} {
		if got := putWithAuthorization(t, srv.URL+"/api/v1/objects/instance-a/backup.gpg", auth); got != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status = %d, want 401", auth, got)
		}
	}

	events, err := db.ListReceiverEvents(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListReceiverEvents() error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("recorded %d events, want one per rejected request: %+v", len(events), events)
	}

	for _, ev := range events {
		if ev.Kind != store.ReceiverEventReject || ev.Success || ev.ReceiverID != "instance-a" || !strings.Contains(ev.Error, "203.0.113.7") {
			t.Errorf("event = %+v, want an unsuccessful reject for instance-a naming the client address", ev)
		}
	}

	if events[0].Key != "backup.gpg" {
		t.Errorf("binding rejection recorded key %q, want backup.gpg", events[0].Key)
	}
}

// putWithAuthorization PUTs a one-byte body to u with auth as its
// Authorization header (none when ""), returning the response status.
func putWithAuthorization(t *testing.T, u, auth string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, u, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}

	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}
