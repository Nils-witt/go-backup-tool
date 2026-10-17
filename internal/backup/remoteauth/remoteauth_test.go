package remoteauth

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testIdentity is a sender's uuid and RSA private key, for tests that need
// one to sign a remote-target request with — a real run instead gets one
// from loadServerIdentity, backed by the persistent 4096-bit key under
// data/keys.
type testIdentity struct {
	uuid       string
	privateKey *rsa.PrivateKey
}

// testServerIdentity builds a testIdentity backed by a fresh (test-speed)
// RSA key.
func testServerIdentity(t *testing.T) *testIdentity {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return &testIdentity{uuid: "test-sender-uuid", privateKey: key}
}

func TestSignAndVerifyRemoteAuthToken(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	token, err := SignRemoteAuthToken(id.privateKey, id.uuid, "receiver-a", Request{})
	if err != nil {
		t.Fatalf("signRemoteAuthToken() unexpected error: %v", err)
	}

	if _, err := VerifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-a", ""); err != nil {
		t.Errorf("verifyRemoteAuthToken() unexpected error: %v", err)
	}
}

func TestRemoteAuthTokenRequestBinding(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)
	want := Request{Method: "PUT", Key: "daily/db.gpg", CreationTime: "2026-01-01T00:00:00Z", BodySHA256: "abc123"}

	token, err := SignRemoteAuthToken(id.privateKey, id.uuid, "receiver-a", want)
	if err != nil {
		t.Fatalf("SignRemoteAuthToken() unexpected error: %v", err)
	}

	got, err := VerifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-a", "")
	if err != nil || got != want {
		t.Errorf("VerifyRemoteAuthToken() = (%+v, %v), want (%+v, nil)", got, err, want)
	}

	unbound, err := SignRemoteAuthToken(id.privateKey, id.uuid, "receiver-a", Request{})
	if err != nil {
		t.Fatalf("SignRemoteAuthToken() unexpected error: %v", err)
	}

	if got, err := VerifyRemoteAuthToken(unbound, &id.privateKey.PublicKey, "receiver-a", ""); err != nil || !got.IsZero() {
		t.Errorf("VerifyRemoteAuthToken(unbound) = (%+v, %v), want a zero Request", got, err)
	}
}

func TestVerifyRemoteAuthTokenWrongPublicKey(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)
	other := testServerIdentity(t)

	token, err := SignRemoteAuthToken(id.privateKey, id.uuid, "receiver-a", Request{})
	if err != nil {
		t.Fatalf("signRemoteAuthToken() unexpected error: %v", err)
	}

	if _, err := VerifyRemoteAuthToken(token, &other.privateKey.PublicKey, "receiver-a", ""); err == nil {
		t.Error("verifyRemoteAuthToken() with the wrong public key = nil error, want one")
	}
}

func TestVerifyRemoteAuthTokenWrongAudience(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	token, err := SignRemoteAuthToken(id.privateKey, id.uuid, "receiver-a", Request{})
	if err != nil {
		t.Fatalf("signRemoteAuthToken() unexpected error: %v", err)
	}

	if _, err := VerifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-b", ""); err == nil {
		t.Error("verifyRemoteAuthToken() with the wrong audience = nil error, want one")
	}
}

func TestVerifyRemoteAuthTokenExpired(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: id.privateKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("building signer: %v", err)
	}

	past := time.Now().Add(-2 * remoteAuthTokenTTL)
	claims := jwt.Claims{
		Issuer:   id.uuid,
		Audience: jwt.Audience{"receiver-a"},
		IssuedAt: jwt.NewNumericDate(past),
		Expiry:   jwt.NewNumericDate(past.Add(time.Minute)),
	}

	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("serializing token: %v", err)
	}

	_, err = VerifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-a", "")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("verifyRemoteAuthToken() with an expired token error = %v, want it to mention expiry", err)
	}
}

func TestVerifyRemoteAuthTokenMalformed(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	if _, err := VerifyRemoteAuthToken("not-a-jwt", &id.privateKey.PublicKey, "receiver-a", ""); err == nil {
		t.Error("verifyRemoteAuthToken() with a malformed token = nil error, want one")
	}
}
