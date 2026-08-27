package backup

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testServerIdentity builds a serverIdentity backed by a fresh (test-speed)
// RSA key, for tests that need a sender's identity to sign a remote-target
// request with — a real run instead gets one from loadServerIdentity, backed
// by the persistent 4096-bit key under data/keys.
func testServerIdentity(t *testing.T) *serverIdentity {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	return &serverIdentity{uuid: "test-sender-uuid", privateKey: key}
}

func TestSignAndVerifyRemoteAuthToken(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	token, err := signRemoteAuthToken(id.privateKey, id.uuid, "receiver-a")
	if err != nil {
		t.Fatalf("signRemoteAuthToken() unexpected error: %v", err)
	}

	if err := verifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-a"); err != nil {
		t.Errorf("verifyRemoteAuthToken() unexpected error: %v", err)
	}
}

func TestVerifyRemoteAuthTokenWrongPublicKey(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)
	other := testServerIdentity(t)

	token, err := signRemoteAuthToken(id.privateKey, id.uuid, "receiver-a")
	if err != nil {
		t.Fatalf("signRemoteAuthToken() unexpected error: %v", err)
	}

	if err := verifyRemoteAuthToken(token, &other.privateKey.PublicKey, "receiver-a"); err == nil {
		t.Error("verifyRemoteAuthToken() with the wrong public key = nil error, want one")
	}
}

func TestVerifyRemoteAuthTokenWrongAudience(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	token, err := signRemoteAuthToken(id.privateKey, id.uuid, "receiver-a")
	if err != nil {
		t.Fatalf("signRemoteAuthToken() unexpected error: %v", err)
	}

	if err := verifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-b"); err == nil {
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

	err = verifyRemoteAuthToken(token, &id.privateKey.PublicKey, "receiver-a")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("verifyRemoteAuthToken() with an expired token error = %v, want it to mention expiry", err)
	}
}

func TestVerifyRemoteAuthTokenMalformed(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)

	if err := verifyRemoteAuthToken("not-a-jwt", &id.privateKey.PublicKey, "receiver-a"); err == nil {
		t.Error("verifyRemoteAuthToken() with a malformed token = nil error, want one")
	}
}
