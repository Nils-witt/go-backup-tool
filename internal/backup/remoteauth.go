package backup

import (
	"crypto/rsa"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// remoteAuthTokenTTL bounds how long a signed remote-target request token
// (see signRemoteAuthToken) stays valid for, keeping a leaked or logged
// token useful to a replay attacker for only a short window. uploadToRemote
// and deleteRemoteObject (pipeline.go) mint a fresh token for every request
// rather than reusing one, so this only needs to cover request/verification
// latency, not an entire backup run.
const remoteAuthTokenTTL = 5 * time.Minute

// SignRemoteAuthToken builds a short-lived RS256-signed JSON Web Token,
// signed with privateKey (this instance's own RSA key — see
// loadServerIdentity), identifying this instance as issuer (its persistent
// UUID — see ensureServerUUID) and scoped to audience (the destination
// receiver's id — a serverKindRemote target's bucket). uploadToRemote/
// deleteRemoteObject send the result as their Authorization: Bearer header;
// the receiving instance verifies it with verifyRemoteAuthToken against
// this instance's public key, configured on its matching receivers: entry
// (see authorizeReceiver in webui.go), replacing the shared bearer token
// previously used for receiver auth.
func SignRemoteAuthToken(privateKey *rsa.PrivateKey, issuer, audience string) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privateKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("building signer: %w", err)
	}

	now := time.Now()

	claims := jwt.Claims{
		Issuer:   issuer,
		Audience: jwt.Audience{audience},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(remoteAuthTokenTTL)),
	}

	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("serializing token: %w", err)
	}

	return token, nil
}

// VerifyRemoteAuthToken parses raw as a JWT, verifies its RS256 signature
// against publicKey (the sender's public key, as configured on the
// matching receivers: entry's public-key: — see fileReceiver.PublicKey),
// and checks it's currently valid (exp/nbf/iat, with jwt.Claims.Validate's
// default one-minute clock-skew leeway) and scoped to audience (this
// receiver's own id). See signRemoteAuthToken for what a sender puts in the
// token.
func VerifyRemoteAuthToken(raw string, publicKey *rsa.PublicKey, audience string) error {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return fmt.Errorf("parsing token: %w", err)
	}

	var claims jwt.Claims
	if err := token.Claims(publicKey, &claims); err != nil {
		return fmt.Errorf("verifying signature: %w", err)
	}

	if err := claims.Validate(jwt.Expected{AnyAudience: jwt.Audience{audience}, Time: time.Now()}); err != nil {
		return fmt.Errorf("validating claims: %w", err)
	}

	return nil
}
