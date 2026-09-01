package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

func TestSaveAndListAPITokensForUser(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	now := time.Now()

	if err := db.SaveAPIToken(ctx, "jti-1", "alice", permission.PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	if err := db.SaveAPIToken(ctx, "jti-2", "alice", permission.PermissionView|permission.PermissionDownload, now.Add(time.Second), now.Add(48*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	if err := db.SaveAPIToken(ctx, "jti-3", "bob", permission.PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	tokens, err := db.ListAPITokensForUser(ctx, "alice")
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

	if tokens[1].Permissions != permission.PermissionView {
		t.Errorf("tokens[1].Permissions = %v, want %v", tokens[1].Permissions, permission.PermissionView)
	}

	if tokens[0].RevokedAt != nil {
		t.Errorf("tokens[0].RevokedAt = %v, want nil (not yet revoked)", tokens[0].RevokedAt)
	}
}

func TestRevokeAPITokenMarksRevokedAndIsIdempotent(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	now := time.Now()

	if err := db.SaveAPIToken(ctx, "jti-1", "alice", permission.PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	first, err := db.RevokeAPIToken(ctx, "jti-1", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RevokeAPIToken() unexpected error: %v", err)
	}

	if first.RevokedAt == nil {
		t.Fatal("RevokeAPIToken() left RevokedAt nil after revoking")
	}

	firstRevokedAt := *first.RevokedAt

	// Revoking an already-revoked token is a no-op, not an error, and
	// doesn't move revoked_at forward.
	second, err := db.RevokeAPIToken(ctx, "jti-1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("RevokeAPIToken() on an already-revoked token: unexpected error: %v", err)
	}

	if second.RevokedAt == nil || !second.RevokedAt.Equal(firstRevokedAt) {
		t.Errorf("RevokeAPIToken() on an already-revoked token changed RevokedAt to %v, want unchanged %v", second.RevokedAt, firstRevokedAt)
	}

	if _, err := db.RevokeAPIToken(ctx, "no-such-jti", now); !errors.Is(err, ErrAPITokenNotFound) {
		t.Errorf("RevokeAPIToken() for an unknown jti = %v, want ErrAPITokenNotFound", err)
	}
}

func TestListRevokedAPITokensExcludesExpiredAndNonRevoked(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	now := time.Now()

	if err := db.SaveAPIToken(ctx, "still-valid-revoked", "alice", permission.PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	if err := db.SaveAPIToken(ctx, "not-revoked", "alice", permission.PermissionView, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	if err := db.SaveAPIToken(ctx, "expired-revoked", "alice", permission.PermissionView, now.Add(-48*time.Hour), now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("SaveAPIToken() unexpected error: %v", err)
	}

	if _, err := db.RevokeAPIToken(ctx, "still-valid-revoked", now); err != nil {
		t.Fatalf("RevokeAPIToken() unexpected error: %v", err)
	}

	if _, err := db.RevokeAPIToken(ctx, "expired-revoked", now); err != nil {
		t.Fatalf("RevokeAPIToken() unexpected error: %v", err)
	}

	revoked, err := db.ListRevokedAPITokens(ctx, now)
	if err != nil {
		t.Fatalf("ListRevokedAPITokens() unexpected error: %v", err)
	}

	if len(revoked) != 1 || revoked[0].JTI != "still-valid-revoked" {
		t.Fatalf("ListRevokedAPITokens() = %+v, want only [still-valid-revoked]", revoked)
	}
}
