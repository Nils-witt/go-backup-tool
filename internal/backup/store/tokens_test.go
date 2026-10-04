package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestTokenSigningKeyIsStable(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	first, err := db.TokenSigningKey(ctx)
	if err != nil {
		t.Fatalf("TokenSigningKey() error: %v", err)
	}

	if len(first) != tokenSigningKeyBytes {
		t.Fatalf("TokenSigningKey() returned %d bytes, want %d", len(first), tokenSigningKeyBytes)
	}

	second, err := db.TokenSigningKey(ctx)
	if err != nil {
		t.Fatalf("TokenSigningKey() second call error: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Error("TokenSigningKey() returned a different key on the second call")
	}
}

// saveTestAPITokens stores tokens "a" then "b", an hour apart.
func saveTestAPITokens(t *testing.T, db *Store, now time.Time) {
	t.Helper()

	for i, id := range []string{"a", "b"} {
		tok := APIToken{ID: id, Name: "tok-" + id, CreatedBy: "admin", CreatedAt: now.Add(time.Duration(i) * time.Hour), ExpiresAt: now.AddDate(0, 0, 30)}
		if err := db.SaveAPIToken(context.Background(), tok); err != nil {
			t.Fatalf("SaveAPIToken(%s) error: %v", id, err)
		}
	}
}

func TestListAPITokensNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	saveTestAPITokens(t, db, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	list, err := db.ListAPITokens(context.Background())
	if err != nil {
		t.Fatalf("ListAPITokens() error: %v", err)
	}

	if len(list) != 2 || list[0].ID != "b" {
		t.Fatalf("ListAPITokens() = %+v, want 2 tokens, newest (b) first", list)
	}
}

func TestRevokeAPIToken(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	saveTestAPITokens(t, db, now)

	ok, err := db.RevokeAPIToken(ctx, "a", "admin", now.Add(2*time.Hour))
	if err != nil || !ok {
		t.Fatalf("RevokeAPIToken(a) = %v, %v; want true, nil", ok, err)
	}

	if ok, _ := db.RevokeAPIToken(ctx, "a", "admin", now); ok {
		t.Error("RevokeAPIToken(a) again = true, want false for an already-revoked token")
	}

	if ok, _ := db.RevokeAPIToken(ctx, "missing", "admin", now); ok {
		t.Error("RevokeAPIToken(missing) = true, want false")
	}

	got, found, err := db.GetAPIToken(ctx, "a")
	if err != nil || !found {
		t.Fatalf("GetAPIToken(a) = found %v, err %v", found, err)
	}

	if got.RevokedAt == nil || got.RevokedBy != "admin" {
		t.Errorf("GetAPIToken(a) = %+v, want revoked by admin", got)
	}

	if _, found, _ := db.GetAPIToken(ctx, "missing"); found {
		t.Error("GetAPIToken(missing) found = true, want false")
	}
}
