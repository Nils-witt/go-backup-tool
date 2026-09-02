package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

func TestStateDBPath(t *testing.T) {
	t.Parallel()

	got := StateDBPath("/etc/go-backup-tool/config.yaml")
	want := filepath.Join("/etc/go-backup-tool", stateDBName)

	if got != want {
		t.Errorf("StateDBPath() = %q, want %q", got, want)
	}
}

// seedLegacyUserTables creates a state db at path with the pre-merge
// webui_users/oidc_user_permissions schema (rather than the current
// usersSchema), seeded with rows Open's migration (see
// ensureUsersMigratedFromLegacyTables) must fold into the unified users
// table — including a pathological case, an OIDC identity ("alice") that
// collides with an existing webui_users username.
func seedLegacyUserTables(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	defer func() { _ = rawDB.Close() }()

	legacySchemas := []string{
		`CREATE TABLE webui_users (
			username      TEXT NOT NULL PRIMARY KEY,
			password_hash TEXT NOT NULL,
			permissions   INTEGER NOT NULL DEFAULT 0,
			created_at    TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE oidc_user_permissions (
			identity    TEXT NOT NULL PRIMARY KEY,
			permissions INTEGER NOT NULL,
			updated_at  TIMESTAMP NOT NULL
		)`,
	}

	for _, schema := range legacySchemas {
		if _, err := rawDB.ExecContext(ctx, schema); err != nil {
			t.Fatalf("creating legacy schema: %v", err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)

	if _, err := rawDB.ExecContext(ctx, `INSERT INTO webui_users (username, password_hash, permissions, created_at) VALUES (?, ?, ?, ?)`,
		"alice", "fake-hash-alice", int(permission.PermissionAdmin), now); err != nil {
		t.Fatalf("seeding webui_users: %v", err)
	}

	if _, err := rawDB.ExecContext(ctx, `INSERT INTO oidc_user_permissions (identity, permissions, updated_at) VALUES (?, ?, ?)`,
		"bob@example.com", int(permission.PermissionView), now); err != nil {
		t.Fatalf("seeding oidc_user_permissions: %v", err)
	}

	if _, err := rawDB.ExecContext(ctx, `INSERT INTO oidc_user_permissions (identity, permissions, updated_at) VALUES (?, ?, ?)`,
		"alice", int(permission.PermissionDownload), now); err != nil {
		t.Fatalf("seeding oidc_user_permissions: %v", err)
	}

	return path
}

func TestOpenMigratesLegacyUserTables(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := seedLegacyUserTables(t)

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	assertMigratedWebUIUser(ctx, t, db)
	assertMigratedOIDCUser(ctx, t, db)
	assertDisambiguatedOIDCCollision(ctx, t, db)
	assertLegacyTablesDropped(ctx, t, db)
}

// assertMigratedWebUIUser checks that the webui_users-derived "alice" row
// migrates untouched and unlinked.
func assertMigratedWebUIUser(ctx context.Context, t *testing.T, db *Store) {
	t.Helper()

	alice, ok, err := db.GetUser(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("GetUser(%q) = (ok=%v, err=%v), want (true, nil)", "alice", ok, err)
	}

	if alice.OIDCUsername != "" {
		t.Errorf("alice.OIDCUsername = %q, want \"\" (a webui_users-derived row must stay unlinked)", alice.OIDCUsername)
	}

	if alice.Permissions != permission.PermissionAdmin {
		t.Errorf("alice.Permissions = %v, want %v", alice.Permissions, permission.PermissionAdmin)
	}
}

// assertMigratedOIDCUser checks that the uncontested oidc_user_permissions
// row migrates linked by its own identity string as both username and
// oidc_username.
func assertMigratedOIDCUser(ctx context.Context, t *testing.T, db *Store) {
	t.Helper()

	bob, ok, err := db.GetUser(ctx, "bob@example.com")
	if err != nil || !ok {
		t.Fatalf("GetUser(%q) = (ok=%v, err=%v), want (true, nil)", "bob@example.com", ok, err)
	}

	if bob.OIDCUsername != "bob@example.com" {
		t.Errorf("bob.OIDCUsername = %q, want %q", bob.OIDCUsername, "bob@example.com")
	}

	if bob.Permissions != permission.PermissionView {
		t.Errorf("bob.Permissions = %v, want %v", bob.Permissions, permission.PermissionView)
	}
}

// assertDisambiguatedOIDCCollision checks that the colliding OIDC identity
// ("alice") never adopts the pre-existing password account's row — it needs
// its own, disambiguated one, still linked via oidc_username to the original
// identity string.
func assertDisambiguatedOIDCCollision(ctx context.Context, t *testing.T, db *Store) {
	t.Helper()

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() error: %v", err)
	}

	var linked *User

	for i := range users {
		if users[i].OIDCUsername == "alice" {
			linked = &users[i]
			break
		}
	}

	if linked == nil {
		t.Fatalf("ListUsers() = %+v, want a row linked to %q", users, "alice")
	}

	if linked.Username == "alice" {
		t.Errorf("colliding row's Username = %q, want a disambiguated name distinct from the pre-existing user", linked.Username)
	}

	if linked.Permissions != permission.PermissionDownload {
		t.Errorf("colliding row's Permissions = %v, want %v", linked.Permissions, permission.PermissionDownload)
	}

	if len(users) != 3 {
		t.Fatalf("ListUsers() = %+v, want exactly 3 rows", users)
	}
}

// assertLegacyTablesDropped checks that the pre-merge legacy tables are gone
// after migration.
func assertLegacyTablesDropped(ctx context.Context, t *testing.T, db *Store) {
	t.Helper()

	for _, table := range []string{"webui_users", "oidc_user_permissions"} {
		exists, err := tableExists(ctx, db.db, table)
		if err != nil {
			t.Fatalf("tableExists(%q): %v", table, err)
		}

		if exists {
			t.Errorf("table %q still exists after migration", table)
		}
	}
}

func TestOpenMigrationIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := seedLegacyUserTables(t)

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open() error: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Reopening an already-migrated db (no legacy tables left) must be a
	// clean no-op, not an error.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() error: %v", err)
	}

	if len(users) != 3 {
		t.Fatalf("ListUsers() after reopening = %+v, want exactly 3 rows (unchanged from the first migration)", users)
	}
}
