package receiver

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/app/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteAuth"
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
	legacy, legacyKey := testTrustedSender(t)

	trusted := trust.NewRegistry(map[string]trust.Server{
		allowed.UUID(): {ID: allowed.UUID(), Name: "allowed", PublicKey: &allowedKey.PublicKey},
		other.UUID():   {ID: other.UUID(), Name: "other", PublicKey: &otherKey.PublicKey},
	})

	recv := config.ResolvedReceiver{ID: "r", AllowedServers: []string{allowed.UUID()}, TrustedServers: trusted}

	sign := func(key *rsa.PrivateKey, issuer, audience string) string {
		t.Helper()

		tok, err := remoteAuth.SignRemoteAuthToken(key, issuer, audience)
		if err != nil {
			t.Fatalf("SignRemoteAuthToken() error: %v", err)
		}

		return tok
	}

	if iss, err := verifyReceiverToken(recv, sign(allowedKey, allowed.UUID(), "r")); err != nil || iss != allowed.UUID() {
		t.Errorf("allowed server: verifyReceiverToken() = %q, %v; want %q, nil", iss, err, allowed.UUID())
	}

	cases := map[string]string{
		"trusted but not allowed":        sign(otherKey, other.UUID(), "r"),
		"allowed issuer, wrong key":      sign(otherKey, allowed.UUID(), "r"),
		"allowed server, wrong audience": sign(allowedKey, allowed.UUID(), "other-receiver"),
		"no legacy key configured":       sign(legacyKey, legacy.UUID(), "r"),
		"garbage":                        "not-a-jwt",
	}

	for name, tok := range cases {
		if _, err := verifyReceiverToken(recv, tok); err == nil {
			t.Errorf("%s: verifyReceiverToken() = nil error, want rejection", name)
		}
	}

	// The deprecated per-receiver key keeps working alongside allowed servers.
	recv.PublicKey = &legacyKey.PublicKey
	if iss, err := verifyReceiverToken(recv, sign(legacyKey, legacy.UUID(), "r")); err != nil || iss != "" {
		t.Errorf("legacy key: verifyReceiverToken() = %q, %v; want \"\", nil", iss, err)
	}

	// Rotating the allowed server's key in the registry applies immediately.
	trusted.Put(trust.Server{ID: allowed.UUID(), Name: "allowed", PublicKey: &otherKey.PublicKey})

	if _, err := verifyReceiverToken(recv, sign(allowedKey, allowed.UUID(), "r")); err == nil {
		t.Error("after key rotation: old key still accepted")
	}

	// Removing it from the registry rejects it even though the receiver
	// still lists it.
	trusted.Delete(allowed.UUID())

	if _, err := verifyReceiverToken(recv, sign(otherKey, allowed.UUID(), "r")); err == nil {
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

	if err := pipeline.UploadToRemote(t.Context(), &config.Config{Key: "a.gpg", Identity: sender}, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("UploadToRemote() from a trusted, allowed server: %v", err)
	}

	stranger, _ := testTrustedSender(t)
	if err := pipeline.UploadToRemote(t.Context(), &config.Config{Key: "b.gpg", Identity: stranger}, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("UploadToRemote() from an unknown server succeeded, want 401")
	}
}
