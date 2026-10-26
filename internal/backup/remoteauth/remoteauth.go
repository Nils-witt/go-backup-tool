// Package remoteauth signs and verifies the short-lived JWTs a backup-tool
// instance uses to authenticate its own requests to a remote target.
package remoteauth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

// remoteAuthTokenTTL bounds how long a signed remote-target request token
// (see SignRemoteAuthToken) stays valid for, keeping a leaked or logged
// token useful to a replay attacker for only a short window.
// remoteclient.Upload mints a fresh token for every request
// rather than reusing one, so this only needs to cover request/verification
// latency, not an entire backup run.
const remoteAuthTokenTTL = 5 * time.Minute

// Request is the one receiver API request a token is bound to: its HTTP
// method, object key, file under that key (for one file of a multi-file
// backup, "" otherwise), creationTime query value (as sent, "" if none)
// and, for a PUT, the hex SHA-256 of its body (plus, for an on-change file,
// its plaintext's — see PlainSHA256). Without it, a token one
// destination instance receives would authorize any method, key, or body
// at any other instance whose receiver has the same id and allows the same
// sender — letting a compromised destination delete or overwrite the
// sender's copies elsewhere. VerifyRemoteAuthToken rejects a token
// binding none of these (see ErrUnboundToken).
type Request struct {
	Method       string `json:"htm,omitempty"`
	Key          string `json:"key,omitempty"`
	File         string `json:"file,omitempty"`
	CreationTime string `json:"ctm,omitempty"`
	BodySHA256   string `json:"sha256,omitempty"`

	// PlainSHA256 is, for an on-change file upload (see
	// remoteclient.UploadFile), the hex SHA-256 of the file's content before
	// encryption, which a directory receiver compares against a path's
	// newest version. "" for every other request.
	PlainSHA256 string `json:"psha,omitempty"`
}

// IsZero reports whether r binds nothing, i.e. came from a token minted by
// a sender that predates request binding.
func (r Request) IsZero() bool {
	return r == Request{}
}

// SignRemoteAuthToken builds a short-lived RS256-signed JSON Web Token,
// signed with privateKey (this instance's own RSA key — see
// loadServerIdentity), identifying this instance as issuer (its persistent
// UUID — see ensureServerUUID), scoped to audience (the destination
// receiver's id — a serverKindRemote target's bucket) and bound to req (see
// Request). remoteclient.Upload sends the result as its
// Authorization: Bearer header; the receiving instance verifies it with
// VerifyRemoteAuthToken against this instance's public key, configured as
// a trusted server allowed on its matching receivers: entry (see
// authorizeReceiver in receiver.go), and checks req against the request it
// arrived with. Every token gets a fresh random jti, which the receiver
// remembers until the token expires, so it can't be used twice.
func SignRemoteAuthToken(privateKey *rsa.PrivateKey, issuer, audience string, req Request) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privateKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("building signer: %w", err)
	}

	now := time.Now()

	claims := jwt.Claims{
		ID:       uuid.NewString(),
		Issuer:   issuer,
		Audience: jwt.Audience{audience},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(remoteAuthTokenTTL)),
	}

	token, err := jwt.Signed(signer).Claims(claims).Claims(req).Serialize()
	if err != nil {
		return "", fmt.Errorf("serializing token: %w", err)
	}

	return token, nil
}

var (
	// ErrUnboundToken is returned by VerifyRemoteAuthToken for a token
	// that binds no request (see Request): one from a sender that predates
	// request binding, which would authorize any method, key and body.
	ErrUnboundToken = errors.New("token is not bound to its request; upgrade the sending instance")

	// ErrNoTokenID is returned by VerifyRemoteAuthToken for a token without
	// a jti, which the receiver needs to reject a replayed token; senders
	// that predate replay protection don't set one.
	ErrNoTokenID = errors.New("token has no jti; upgrade the sending instance")
)

// Token is what VerifyRemoteAuthToken returns about a verified token.
type Token struct {
	// Request is the request the token is bound to, which the caller must
	// check against the actual request. Never zero.
	Request Request

	// ID is the token's jti and Expiry its exp, for the caller to remember
	// ID until Expiry (plus jwt.DefaultLeeway) and reject it if it comes
	// back. ID is never "".
	ID     string
	Expiry time.Time
}

// VerifyRemoteAuthToken parses raw as a JWT, verifies its RS256 signature
// against publicKey (the sender's public key, from its trusted server
// entry), and checks it's currently valid
// (exp/nbf/iat, with jwt.Claims.Validate's default one-minute clock-skew
// leeway), scoped to audience (this receiver's own id), and — unless issuer
// is "" — issued by issuer (the trusted server's id). The token must carry
// a jti (ErrNoTokenID) and be bound to a request (ErrUnboundToken). See
// SignRemoteAuthToken for what a sender puts in the token. Checking that
// the jti hasn't been seen before is the caller's job.
func VerifyRemoteAuthToken(raw string, publicKey *rsa.PublicKey, audience, issuer string) (Token, error) {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return Token{}, fmt.Errorf("parsing token: %w", err)
	}

	var (
		claims jwt.Claims
		req    Request
	)

	if err := token.Claims(publicKey, &claims, &req); err != nil {
		return Token{}, fmt.Errorf("verifying signature: %w", err)
	}

	if err := claims.Validate(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{audience}, Time: time.Now()}); err != nil {
		return Token{}, fmt.Errorf("validating claims: %w", err)
	}

	switch {
	case claims.ID == "":
		return Token{}, ErrNoTokenID
	case req.IsZero():
		return Token{}, ErrUnboundToken
	case claims.Expiry == nil:
		return Token{}, errors.New("token has no expiry")
	}

	return Token{Request: req, ID: claims.ID, Expiry: claims.Expiry.Time()}, nil
}

// UnverifiedIssuer returns raw's iss claim WITHOUT verifying its signature,
// solely to pick which trusted server's key to verify raw against; the
// result must never be trusted until VerifyRemoteAuthToken has checked raw
// against that key with the same issuer.
func UnverifiedIssuer(raw string) (string, error) {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return "", fmt.Errorf("parsing token: %w", err)
	}

	var claims jwt.Claims
	if err := token.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", fmt.Errorf("reading claims: %w", err)
	}

	return claims.Issuer, nil
}
