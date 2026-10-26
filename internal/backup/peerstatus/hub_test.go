package peerstatus

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

var discardLogger = slog.New(slog.DiscardHandler)

// testIdentity returns a fresh instance identity with a UUID server id.
func testIdentity(t *testing.T) *identity.ServerIdentity {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return identity.NewTestServerIdentity(uuid.NewString(), key)
}

// openTestStateDB opens a fresh SQLite state db, closed when the test ends.
func openTestStateDB(t *testing.T) store.Store {
	t.Helper()

	db, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// testHub is a Hub trusting sender, served over HTTP.
type testHub struct {
	*Hub

	db      store.Store
	trusted *trust.Registry
	url     string
}

func newTestHub(t *testing.T, sender *identity.ServerIdentity) testHub {
	t.Helper()

	trusted := trust.NewRegistry(map[string]trust.Server{
		sender.UUID(): {ID: sender.UUID(), Name: "branch-office", PublicKey: &sender.PrivateKey().PublicKey},
	})
	db := openTestStateDB(t)
	hub := NewHub(db, trusted, discardLogger)

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux, nil)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return testHub{Hub: hub, db: db, trusted: trusted, url: srv.URL}
}

// target returns h as a type: remote server's target.
func (h testHub) target() *config.Target {
	return &config.Target{ServerName: "central", Kind: config.ServerKindRemote, Endpoint: h.url}
}

func testPayload() Payload {
	return Payload{
		Version:         Version,
		InstanceName:    "branch",
		IntervalSeconds: 30,
		SentAt:          time.Now(),
		Jobs:            []status.JobSnapshot{{Name: "db", State: status.StateOK, Targets: []status.TargetSnapshot{}}},
		Receivers:       []status.ReceiverSnapshot{{ID: "inbox", State: status.StateIdle}},
	}
}

func encode(t *testing.T, p Payload) []byte {
	t.Helper()

	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encoding payload: %v", err)
	}

	return body
}

// postSigned POSTs body to h's push endpoint with token, returning the
// response status.
func postSigned(t *testing.T, h testHub, token string, body []byte) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+remoteclient.StatusPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// pushToken signs a push token for body, scoped to audience.
func pushToken(t *testing.T, id *identity.ServerIdentity, audience string, body []byte) string {
	t.Helper()

	sum := sha256.Sum256(body)

	token, err := id.SignRequest(audience, remoteauth.Request{
		Method: http.MethodPost, Key: remoteclient.StatusKey, BodySHA256: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	return token
}

func TestHubAcceptsPush(t *testing.T) {
	t.Parallel()

	sender := testIdentity(t)
	h := newTestHub(t, sender)
	changed := h.Changed()

	if err := remoteclient.PostStatus(t.Context(), sender, h.target(), encode(t, testPayload())); err != nil {
		t.Fatalf("PostStatus() error: %v", err)
	}

	select {
	case <-changed:
	default:
		t.Error("Changed() didn't fire on a push")
	}

	assertSamplePushed(t, h.Snapshot(), sender)

	stored, err := h.db.ListPeerStatuses(t.Context())
	if err != nil || len(stored) != 1 || stored[0].ServerID != sender.UUID() {
		t.Errorf("ListPeerStatuses() = (%+v, %v), want the push stored", stored, err)
	}
}

// assertSamplePushed checks that snap is testPayload, pushed by sender.
func assertSamplePushed(t *testing.T, snap []status.PeerSnapshot, sender *identity.ServerIdentity) {
	t.Helper()

	if len(snap) != 1 {
		t.Fatalf("Snapshot() = %+v, want one instance", snap)
	}

	got := snap[0]
	if got.ID != sender.UUID() || got.Name != "branch-office" || got.InstanceName != "branch" || got.Interval != "30s" || got.Offline {
		t.Errorf("Snapshot()[0] = %+v, want branch-office/branch, 30s, online", got)
	}

	if len(got.Jobs) != 1 || got.Jobs[0].Name != "db" || len(got.Receivers) != 1 || got.Receivers[0].ID != "inbox" {
		t.Errorf("Snapshot()[0] jobs/receivers = %+v / %+v, want db / inbox", got.Jobs, got.Receivers)
	}
}

func TestHubRejectsPush(t *testing.T) {
	t.Parallel()

	sender := testIdentity(t)
	h := newTestHub(t, sender)
	body := encode(t, testPayload())

	tooLarge := encode(t, Payload{Version: Version, InstanceName: strings.Repeat("x", maxBody)})
	wrongVersion := encode(t, Payload{Version: Version + 1})

	tests := []struct {
		name     string
		token    string
		body     []byte
		expected int
	}{
		{name: "no token", body: body, expected: http.StatusUnauthorized},
		{name: "untrusted sender", token: pushToken(t, testIdentity(t), remoteclient.StatusAudience, body), body: body, expected: http.StatusUnauthorized},
		{name: "receiver upload audience", token: pushToken(t, sender, "inbox", body), body: body, expected: http.StatusUnauthorized},
		{name: "tampered body", token: pushToken(t, sender, remoteclient.StatusAudience, body), body: encode(t, Payload{Version: Version}), expected: http.StatusUnauthorized},
		{name: "too large", token: pushToken(t, sender, remoteclient.StatusAudience, tooLarge), body: tooLarge, expected: http.StatusRequestEntityTooLarge},
		{name: "unsupported version", token: pushToken(t, sender, remoteclient.StatusAudience, wrongVersion), body: wrongVersion, expected: http.StatusBadRequest},
	}

	// Runs once every subtest below is done.
	t.Cleanup(func() {
		if snap := h.Snapshot(); len(snap) != 0 {
			t.Errorf("Snapshot() after rejected pushes = %+v, want empty", snap)
		}
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := postSigned(t, h, tt.token, tt.body); got != tt.expected {
				t.Errorf("POST status = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestHubRejectsReplayedToken(t *testing.T) {
	t.Parallel()

	sender := testIdentity(t)
	h := newTestHub(t, sender)
	body := encode(t, testPayload())
	token := pushToken(t, sender, remoteclient.StatusAudience, body)

	if got := postSigned(t, h, token, body); got != http.StatusNoContent {
		t.Fatalf("first POST status = %d, want 204", got)
	}

	if got := postSigned(t, h, token, body); got != http.StatusUnauthorized {
		t.Errorf("replayed POST status = %d, want 401", got)
	}
}

func TestHubUntrustedDropsOut(t *testing.T) {
	t.Parallel()

	sender := testIdentity(t)
	h := newTestHub(t, sender)

	if err := remoteclient.PostStatus(t.Context(), sender, h.target(), encode(t, testPayload())); err != nil {
		t.Fatalf("PostStatus() error: %v", err)
	}

	h.trusted.Delete(sender.UUID())

	if snap := h.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot() once untrusted = %+v, want empty", snap)
	}

	// The next startup deletes its stored status.
	reloaded := NewHub(h.db, h.trusted, discardLogger)
	reloaded.Load(t.Context())

	if stored, err := h.db.ListPeerStatuses(t.Context()); err != nil || len(stored) != 0 {
		t.Errorf("ListPeerStatuses() after Load = (%+v, %v), want empty", stored, err)
	}
}

func TestHubLoad(t *testing.T) {
	t.Parallel()

	sender := testIdentity(t)
	h := newTestHub(t, sender)

	if err := remoteclient.PostStatus(t.Context(), sender, h.target(), encode(t, testPayload())); err != nil {
		t.Fatalf("PostStatus() error: %v", err)
	}

	if err := h.db.SavePeerStatus(t.Context(), store.PeerStatus{ServerID: "garbage", ReceivedAt: time.Now(), Payload: []byte("{")}); err != nil {
		t.Fatal(err)
	}

	h.trusted.Put(trust.Server{ID: "garbage", Name: "garbage"})

	reloaded := NewHub(h.db, h.trusted, discardLogger)
	reloaded.Load(t.Context())

	snap := reloaded.Snapshot()
	if len(snap) != 1 || snap[0].ID != sender.UUID() || len(snap[0].Jobs) != 1 {
		t.Errorf("Snapshot() after Load = %+v, want the stored push only", snap)
	}

	if stored, err := h.db.ListPeerStatuses(t.Context()); err != nil || len(stored) != 1 {
		t.Errorf("ListPeerStatuses() after Load = (%+v, %v), want the unreadable row deleted", stored, err)
	}
}

func TestHubForget(t *testing.T) {
	t.Parallel()

	sender := testIdentity(t)
	h := newTestHub(t, sender)

	if err := remoteclient.PostStatus(t.Context(), sender, h.target(), encode(t, testPayload())); err != nil {
		t.Fatalf("PostStatus() error: %v", err)
	}

	if known, err := h.Forget(t.Context(), sender.UUID()); err != nil || !known {
		t.Fatalf("Forget() = (%v, %v), want (true, nil)", known, err)
	}

	if snap := h.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot() after Forget = %+v, want empty", snap)
	}

	if stored, err := h.db.ListPeerStatuses(t.Context()); err != nil || len(stored) != 0 {
		t.Errorf("ListPeerStatuses() after Forget = (%+v, %v), want empty", stored, err)
	}

	if known, err := h.Forget(t.Context(), sender.UUID()); err != nil || known {
		t.Errorf("Forget() again = (%v, %v), want (false, nil)", known, err)
	}
}

func TestHubNil(t *testing.T) {
	t.Parallel()

	var h *Hub

	if snap := h.Snapshot(); snap == nil || len(snap) != 0 {
		t.Errorf("nil hub Snapshot() = %#v, want an empty, non-nil slice", snap)
	}
}
