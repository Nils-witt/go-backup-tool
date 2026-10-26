package remoteauth

import (
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
)

func TestReplayCache(t *testing.T) {
	t.Parallel()

	var c ReplayCache

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(5 * time.Minute)

	if !c.Use("sender-a", "jti-1", expiry, now) {
		t.Fatal("first use of a token = false, want true")
	}

	if c.Use("sender-a", "jti-1", expiry, now.Add(time.Minute)) {
		t.Error("second use of the same token = true, want false")
	}

	if !c.Use("sender-b", "jti-1", expiry, now) {
		t.Error("same jti from another sender = false, want true (jti is only unique per sender)")
	}

	if c.Use("sender-a", "jti-1", expiry, expiry.Add(jwt.DefaultLeeway)) {
		t.Error("token reused within the clock-skew leeway after expiry = true, want false")
	}

	if _, ok := c.seen["sender-a\x00jti-1"]; !ok {
		t.Fatal("token forgotten before its leeway passed")
	}

	c.Use("sender-c", "jti-2", expiry, expiry.Add(jwt.DefaultLeeway+replaySweepInterval))

	if _, ok := c.seen["sender-a\x00jti-1"]; ok {
		t.Error("expired token still remembered after a sweep")
	}
}
