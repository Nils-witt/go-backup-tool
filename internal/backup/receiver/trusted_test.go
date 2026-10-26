package receiver

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// testTrustedSender returns a fresh sender identity with a UUID server id,
// plus its key.
func testTrustedSender(t *testing.T) (*identity.ServerIdentity, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return identity.NewTestServerIdentity(uuid.NewString(), key), key
}

func TestVerifyReceiverTokenTrustedServers(t *testing.T) {
	t.Parallel()

	allowed, allowedKey := testTrustedSender(t)
	other, otherKey := testTrustedSender(t)
	stranger, strangerKey := testTrustedSender(t)

	trusted := trust.NewRegistry(map[string]trust.Server{
		allowed.UUID(): {ID: allowed.UUID(), Name: "allowed", PublicKey: &allowedKey.PublicKey},
		other.UUID():   {ID: other.UUID(), Name: "other", PublicKey: &otherKey.PublicKey},
	})

	recv := config.ResolvedReceiver{ID: "r", AllowedServers: []string{allowed.UUID()}, TrustedServers: trusted}

	sign := func(key *rsa.PrivateKey, issuer, audience string) string {
		t.Helper()

		tok, err := remoteauth.SignRemoteAuthToken(key, issuer, audience, remoteauth.Request{Method: http.MethodPut, Key: "k", BodySHA256: "abc"})
		if err != nil {
			t.Fatalf("SignRemoteAuthToken() error: %v", err)
		}

		return tok
	}

	if _, _, err := verifyReceiverToken(recv, sign(allowedKey, allowed.UUID(), "r")); err != nil {
		t.Errorf("allowed server: verifyReceiverToken() error: %v", err)
	}

	cases := map[string]string{
		"trusted but not allowed":        sign(otherKey, other.UUID(), "r"),
		"allowed issuer, wrong key":      sign(otherKey, allowed.UUID(), "r"),
		"allowed server, wrong audience": sign(allowedKey, allowed.UUID(), "other-receiver"),
		"sender not trusted":             sign(strangerKey, stranger.UUID(), "r"),
		"garbage":                        "not-a-jwt",
	}

	for name, tok := range cases {
		if _, _, err := verifyReceiverToken(recv, tok); err == nil {
			t.Errorf("%s: verifyReceiverToken() = nil error, want rejection", name)
		}
	}

	// Rotating the allowed server's key in the registry applies immediately.
	trusted.Put(trust.Server{ID: allowed.UUID(), Name: "allowed", PublicKey: &otherKey.PublicKey})

	if _, _, err := verifyReceiverToken(recv, sign(allowedKey, allowed.UUID(), "r")); err == nil {
		t.Error("after key rotation: old key still accepted")
	}

	// Removing it from the registry rejects it even though the receiver
	// still lists it.
	trusted.Delete(allowed.UUID())

	if _, _, err := verifyReceiverToken(recv, sign(otherKey, allowed.UUID(), "r")); err == nil {
		t.Error("after removal from the registry: token still accepted")
	}
}

func TestRemoteTargetInteropWithTrustedServer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sender, key := testTrustedSender(t)
	trusted := trust.NewRegistry(map[string]trust.Server{
		sender.UUID(): {ID: sender.UUID(), Name: "sender", PublicKey: &key.PublicKey},
	})
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", AllowedServers: []string{sender.UUID()}, TrustedServers: trusted, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := remoteclient.Upload(t.Context(), sender, &config.Config{Key: "a.gpg"}, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("remoteclient.Upload() from a trusted, allowed server: %v", err)
	}

	stranger, _ := testTrustedSender(t)
	if err := remoteclient.Upload(t.Context(), stranger, &config.Config{Key: "b.gpg"}, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("remoteclient.Upload() from an unknown server succeeded, want 401")
	}
}

// TestReceiverTokenReplayRejected checks a token sender S sent instance A
// for one PUT can't be replayed by A against another instance B whose
// receiver has the same id and also allows S: not as a DELETE, not for
// another key or creationTime, and not with a different body — nor sent to
// A itself a second time. Once B has verified the token for one replay, it
// remembers its jti, so later replays fail as replays (401) whatever they
// change.
func TestReceiverTokenReplayRejected(t *testing.T) {
	t.Parallel()

	sender, key := testTrustedSender(t)
	receivers := func(dir string) map[string]config.ResolvedReceiver {
		trusted := trust.NewRegistry(map[string]trust.Server{
			sender.UUID(): {ID: sender.UUID(), Name: "sender", PublicKey: &key.PublicKey},
		})

		return map[string]config.ResolvedReceiver{
			"from-primary": {ID: "from-primary", AllowedServers: []string{sender.UUID()}, TrustedServers: trusted, Path: dir},
		}
	}

	var captured string

	muxA := newReceiverMux(receivers(t.TempDir()))

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("Authorization")
		muxA.ServeHTTP(w, r)
	}))
	defer srvA.Close()

	dirB := t.TempDir()

	srvB := httptest.NewServer(newReceiverMux(receivers(dirB)))
	defer srvB.Close()

	cfg := &config.Config{Key: "db.gpg", CreatedAt: time.Now().Add(-time.Minute)}

	if err := remoteclient.Upload(t.Context(), sender, cfg, &config.Target{Kind: config.ServerKindRemote, Endpoint: srvB.URL, Bucket: "from-primary"}, strings.NewReader("genuine")); err != nil {
		t.Fatalf("remoteclient.Upload(B): %v", err)
	}

	if err := remoteclient.Upload(t.Context(), sender, cfg, &config.Target{Kind: config.ServerKindRemote, Endpoint: srvA.URL, Bucket: "from-primary"}, strings.NewReader("genuine")); err != nil {
		t.Fatalf("remoteclient.Upload(A): %v", err)
	}

	creationTime := url.Values{"creationTime": {cfg.CreatedAt.UTC().Format(time.RFC3339)}}.Encode()
	backdated := url.Values{"creationTime": {"2000-01-01T00:00:00Z"}}.Encode()

	tests := []struct {
		name     string
		target   string // srvB.URL when ""
		method   string
		path     string
		body     string
		expected int
	}{
		{name: "delete", method: http.MethodDelete, path: "db.gpg", expected: http.StatusMethodNotAllowed},
		{name: "other key", method: http.MethodPut, path: "other.gpg?" + creationTime, body: "genuine", expected: http.StatusUnauthorized},
		{name: "backdated creation time", method: http.MethodPut, path: "db.gpg?" + backdated, body: "genuine", expected: http.StatusUnauthorized},
		{name: "different body", method: http.MethodPut, path: "db.gpg?" + creationTime, body: "attacker", expected: http.StatusUnauthorized},
		{name: "same request to A", target: srvA.URL, method: http.MethodPut, path: "db.gpg?" + creationTime, body: "genuine", expected: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		base := srvB.URL
		if tt.target != "" {
			base = tt.target
		}

		req, err := http.NewRequestWithContext(t.Context(), tt.method, base+"/api/v1/objects/from-primary/"+tt.path, strings.NewReader(tt.body))
		if err != nil {
			t.Fatalf("%s: building request: %v", tt.name, err)
		}

		req.Header.Set("Authorization", captured)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: sending request: %v", tt.name, err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != tt.expected {
			t.Errorf("%s replayed to B = %d, want %d", tt.name, resp.StatusCode, tt.expected)
		}
	}

	got, err := os.ReadFile(filepath.Join(dirB, "db.gpg")) //nolint:gosec // path is under the test's own temp dir
	if err != nil || string(got) != "genuine" {
		t.Errorf("B's db.gpg after replays = (%q, %v), want the genuine upload untouched", got, err)
	}
}
