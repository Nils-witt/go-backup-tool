package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecordAndListAPITokensForUser(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	now := time.Now()

	if err := RecordAPIToken(ctx, db, "jti-1", "alice", PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	if err := RecordAPIToken(ctx, db, "jti-2", "alice", PermissionView|PermissionDownload, now.Add(time.Second), now.Add(48*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	if err := RecordAPIToken(ctx, db, "jti-3", "bob", PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	tokens, err := ListAPITokensForUser(ctx, db, "alice")
	if err != nil {
		t.Fatalf("ListAPITokensForUser() unexpected error: %v", err)
	}

	if len(tokens) != 2 {
		t.Fatalf("ListAPITokensForUser() returned %d tokens, want 2", len(tokens))
	}

	// Most recently issued first.
	if tokens[0].JTI != "jti-2" || tokens[1].JTI != "jti-1" {
		t.Errorf("ListAPITokensForUser() order = [%s, %s], want [jti-2, jti-1]", tokens[0].JTI, tokens[1].JTI)
	}

	if tokens[1].Permissions != PermissionView {
		t.Errorf("tokens[1].Permissions = %v, want %v", tokens[1].Permissions, PermissionView)
	}

	if tokens[0].RevokedAt != nil {
		t.Errorf("tokens[0].RevokedAt = %v, want nil (not yet revoked)", tokens[0].RevokedAt)
	}
}

func TestRevokeAPITokenMarksRevokedAndIsIdempotent(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	now := time.Now()

	if err := RecordAPIToken(ctx, db, "jti-1", "alice", PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	first, err := RevokeAPIToken(ctx, db, "jti-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RevokeAPIToken() unexpected error: %v", err)
	}

	if first.RevokedAt == nil {
		t.Fatal("RevokeAPIToken() left RevokedAt nil after revoking")
	}

	firstRevokedAt := *first.RevokedAt

	// Revoking an already-revoked token is a no-op, not an error, and
	// doesn't move revoked_at forward.
	second, err := RevokeAPIToken(ctx, db, "jti-1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("RevokeAPIToken() on an already-revoked token: unexpected error: %v", err)
	}

	if second.RevokedAt == nil || !second.RevokedAt.Equal(firstRevokedAt) {
		t.Errorf("RevokeAPIToken() on an already-revoked token changed RevokedAt to %v, want unchanged %v", second.RevokedAt, firstRevokedAt)
	}

	if _, err := RevokeAPIToken(ctx, db, "no-such-jti", now); !errors.Is(err, ErrAPITokenNotFound) {
		t.Errorf("RevokeAPIToken() for an unknown jti = %v, want ErrAPITokenNotFound", err)
	}
}

func TestListRevokedAPITokensExcludesExpiredAndNonRevoked(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	now := time.Now()

	if err := RecordAPIToken(ctx, db, "still-valid-revoked", "alice", PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	if err := RecordAPIToken(ctx, db, "not-revoked", "alice", PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	if err := RecordAPIToken(ctx, db, "expired-revoked", "alice", PermissionView, now.Add(-48*time.Hour), now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("RecordAPIToken() unexpected error: %v", err)
	}

	if _, err := RevokeAPIToken(ctx, db, "still-valid-revoked", now); err != nil {
		t.Fatalf("RevokeAPIToken() unexpected error: %v", err)
	}

	if _, err := RevokeAPIToken(ctx, db, "expired-revoked", now); err != nil {
		t.Fatalf("RevokeAPIToken() unexpected error: %v", err)
	}

	revoked, err := ListRevokedAPITokens(ctx, db, now)
	if err != nil {
		t.Fatalf("ListRevokedAPITokens() unexpected error: %v", err)
	}

	if len(revoked) != 1 || revoked[0].JTI != "still-valid-revoked" {
		t.Fatalf("ListRevokedAPITokens() = %+v, want only [still-valid-revoked]", revoked)
	}
}
