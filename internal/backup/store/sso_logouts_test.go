package store

import (
	"context"
	"testing"
	"time"
)

func TestListSSOLogoutsOnlyUnexpiredForIssuer(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	logouts := []SSOLogout{
		{Issuer: "https://idp", Subject: "alice", SessionID: "s1", LoggedOutAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
		{Issuer: "https://idp", Subject: "bob", LoggedOutAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Minute)},
		{Issuer: "https://other", Subject: "carol", LoggedOutAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)},
	}

	for _, l := range logouts {
		if err := db.SaveSSOLogout(ctx, l); err != nil {
			t.Fatalf("SaveSSOLogout(%s) error: %v", l.Subject, err)
		}
	}

	got, err := db.ListSSOLogouts(ctx, "https://idp", now)
	if err != nil {
		t.Fatalf("ListSSOLogouts() error: %v", err)
	}

	if len(got) != 1 || got[0].Subject != "alice" || got[0].SessionID != "s1" || !got[0].LoggedOutAt.Equal(logouts[0].LoggedOutAt) {
		t.Fatalf("ListSSOLogouts() = %+v, want only alice's unexpired logout", got)
	}
}

func TestDeleteExpiredSSOLogouts(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for i, expires := range []time.Time{now.Add(-time.Minute), now.Add(time.Minute)} {
		l := SSOLogout{Issuer: "https://idp", SessionID: string(rune('a' + i)), LoggedOutAt: now.Add(-time.Hour), ExpiresAt: expires}
		if err := db.SaveSSOLogout(ctx, l); err != nil {
			t.Fatalf("SaveSSOLogout() error: %v", err)
		}
	}

	if err := db.DeleteExpiredSSOLogouts(ctx, now); err != nil {
		t.Fatalf("DeleteExpiredSSOLogouts() error: %v", err)
	}

	// Listing as of long ago would include both, had neither been deleted.
	got, err := db.ListSSOLogouts(ctx, "https://idp", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("ListSSOLogouts() error: %v", err)
	}

	if len(got) != 1 || got[0].SessionID != "b" {
		t.Fatalf("after DeleteExpiredSSOLogouts, ListSSOLogouts() = %+v, want only the unexpired logout b", got)
	}
}
