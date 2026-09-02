package store

import (
	"context"
	"errors"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

func TestSaveAndVerifyUser(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	perm, ok, err := db.VerifyUser(ctx, "alice", "hunter2")
	if err != nil {
		t.Fatalf("VerifyUser() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("VerifyUser() ok = false, want true for the correct password")
	}

	if perm != permission.PermissionView {
		t.Errorf("VerifyUser() perm = %v, want %v", perm, permission.PermissionView)
	}

	if _, ok, err := db.VerifyUser(ctx, "alice", "wrong"); err != nil || ok {
		t.Errorf("VerifyUser() with wrong password = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := db.VerifyUser(ctx, "bob", "hunter2"); err != nil || ok {
		t.Errorf("VerifyUser() for unknown user = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestSaveUserRejectsDuplicateUsername(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveUser(ctx, "alice", "different", "", permission.PermissionDownload); !errors.Is(err, ErrUserExists) {
		t.Errorf("SaveUser() with a duplicate username = %v, want ErrUserExists", err)
	}
}

func TestSaveUserRejectsDuplicateOIDCUsername(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveUser(ctx, "bob", "hunter3", "alice@example.com", permission.PermissionView); !errors.Is(err, ErrOIDCUsernameTaken) {
		t.Errorf("SaveUser() with a duplicate oidc_username = %v, want ErrOIDCUsernameTaken", err)
	}
}

func TestUpdateUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.UpdateUserPermissions(ctx, "alice", permission.PermissionView|permission.PermissionDownload); err != nil {
		t.Fatalf("UpdateUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := db.VerifyUser(ctx, "alice", "hunter2")
	if err != nil || !ok {
		t.Fatalf("VerifyUser() after permission update = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := permission.PermissionView | permission.PermissionDownload; perm != want {
		t.Errorf("VerifyUser() perm after update = %v, want %v", perm, want)
	}

	if err := db.UpdateUserPermissions(ctx, "nobody", permission.PermissionView); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("UpdateUserPermissions() for unknown user = %v, want ErrUserNotFound", err)
	}
}

func TestUpdateUserPassword(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.UpdateUserPassword(ctx, "alice", "newpass"); err != nil {
		t.Fatalf("UpdateUserPassword() unexpected error: %v", err)
	}

	if _, ok, err := db.VerifyUser(ctx, "alice", "hunter2"); err != nil || ok {
		t.Errorf("VerifyUser() with the old password after a change = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := db.VerifyUser(ctx, "alice", "newpass"); err != nil || !ok {
		t.Errorf("VerifyUser() with the new password = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
}

func TestVerifyUserFailsForOIDCOnlyAccount(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if _, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	if _, ok, err := db.VerifyUser(ctx, "alice@example.com", ""); err != nil || ok {
		t.Errorf("VerifyUser() for an OIDC-only account = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := db.VerifyUser(ctx, "alice@example.com", "anything"); err != nil || ok {
		t.Errorf("VerifyUser() for an OIDC-only account with a guessed password = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestSetUserOIDCUsernameLinksAndClears(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SetUserOIDCUsername(ctx, "alice", "alice@example.com"); err != nil {
		t.Fatalf("SetUserOIDCUsername() unexpected error: %v", err)
	}

	user, ok, err := db.GetUser(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("GetUser() after linking = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if user.OIDCUsername != "alice@example.com" {
		t.Errorf("OIDCUsername after linking = %q, want %q", user.OIDCUsername, "alice@example.com")
	}

	if err := db.SetUserOIDCUsername(ctx, "alice", ""); err != nil {
		t.Fatalf("SetUserOIDCUsername() unexpected error clearing: %v", err)
	}

	user, ok, err = db.GetUser(ctx, "alice")
	if err != nil || !ok {
		t.Fatalf("GetUser() after clearing = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if user.OIDCUsername != "" {
		t.Errorf("OIDCUsername after clearing = %q, want \"\"", user.OIDCUsername)
	}

	if err := db.SetUserOIDCUsername(ctx, "nobody", "x@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetUserOIDCUsername() for unknown user = %v, want ErrUserNotFound", err)
	}
}

func TestSetUserOIDCUsernameRejectsDuplicate(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveUser(ctx, "bob", "hunter3", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SetUserOIDCUsername(ctx, "bob", "alice@example.com"); !errors.Is(err, ErrOIDCUsernameTaken) {
		t.Errorf("SetUserOIDCUsername() with an already-claimed identity = %v, want ErrOIDCUsernameTaken", err)
	}
}

func TestDeleteUser(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser() unexpected error: %v", err)
	}

	if _, ok, err := db.VerifyUser(ctx, "alice", "hunter2"); err != nil || ok {
		t.Errorf("VerifyUser() after deletion = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := db.DeleteUser(ctx, "alice"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("DeleteUser() for an already-deleted user = %v, want ErrUserNotFound", err)
	}
}

func TestListUsersOrdersByUsernameAndIncludesOIDCUsername(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "bob", "pw1", "", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveUser(ctx, "alice", "pw2", "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() unexpected error: %v", err)
	}

	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("ListUsers() = %+v, want [alice, bob] in that order", users)
	}

	if users[0].Permissions != permission.PermissionView {
		t.Errorf("users[0].Permissions = %v, want %v", users[0].Permissions, permission.PermissionView)
	}

	if users[0].OIDCUsername != "alice@example.com" {
		t.Errorf("users[0].OIDCUsername = %q, want %q", users[0].OIDCUsername, "alice@example.com")
	}

	if users[1].Permissions != permission.PermissionDownload {
		t.Errorf("users[1].Permissions = %v, want %v", users[1].Permissions, permission.PermissionDownload)
	}

	if users[1].OIDCUsername != "" {
		t.Errorf("users[1].OIDCUsername = %q, want \"\"", users[1].OIDCUsername)
	}
}

func TestGetOrProvisionOIDCUserFirstLoginProvisions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if _, ok, err := db.GetUser(ctx, "alice@example.com"); err != nil || ok {
		t.Fatalf("GetUser() before first login = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	perm, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView)
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	if perm != permission.PermissionView {
		t.Errorf("GetOrProvisionOIDCUser() perm = %v, want %v", perm, permission.PermissionView)
	}

	user, ok, err := db.GetUser(ctx, "alice@example.com")
	if err != nil || !ok {
		t.Fatalf("GetUser() after first login = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if user.OIDCUsername != "alice@example.com" {
		t.Errorf("provisioned OIDCUsername = %q, want %q", user.OIDCUsername, "alice@example.com")
	}
}

func TestGetOrProvisionOIDCUserReturnsStoredOverrideOnSubsequentLogin(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if _, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView|permission.PermissionDownload); err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	// An admin restricts it to view-only.
	if err := db.UpdateUserPermissions(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("UpdateUserPermissions() unexpected error: %v", err)
	}

	// A later login must honor the admin's edit, not the original default.
	perm, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView|permission.PermissionDownload)
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	if perm != permission.PermissionView {
		t.Errorf("GetOrProvisionOIDCUser() perm on later login = %v, want %v (the admin's edit)", perm, permission.PermissionView)
	}
}

func TestGetOrProvisionOIDCUserNeverAdoptsUsernameMatchedRow(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice@example.com", "hunter2", "", permission.PermissionAdmin); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	perm, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView)
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	if perm != permission.PermissionView {
		t.Errorf("GetOrProvisionOIDCUser() perm = %v, want %v (auth.defaultPerm, not the unrelated password account's)", perm, permission.PermissionView)
	}

	// The pre-existing password account must be untouched.
	existing, ok, err := db.GetUser(ctx, "alice@example.com")
	if err != nil || !ok {
		t.Fatalf("GetUser(%q) = (ok=%v, err=%v), want (true, nil)", "alice@example.com", ok, err)
	}

	if existing.OIDCUsername != "" {
		t.Errorf("pre-existing user's OIDCUsername = %q, want \"\" (untouched)", existing.OIDCUsername)
	}

	if existing.Permissions != permission.PermissionAdmin {
		t.Errorf("pre-existing user's Permissions = %v, want %v (untouched)", existing.Permissions, permission.PermissionAdmin)
	}
}

func TestGetOrProvisionOIDCUserDisambiguatesUsernameOnCollision(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice@example.com", "hunter2", "", permission.PermissionAdmin); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if _, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() unexpected error: %v", err)
	}

	var linked *User

	for i := range users {
		if users[i].OIDCUsername == "alice@example.com" {
			linked = &users[i]
			break
		}
	}

	if linked == nil {
		t.Fatalf("ListUsers() = %+v, want a row linked to %q", users, "alice@example.com")
	}

	if linked.Username == "alice@example.com" {
		t.Errorf("provisioned row's Username = %q, want a disambiguated name distinct from the pre-existing user", linked.Username)
	}

	if len(users) != 2 {
		t.Errorf("ListUsers() = %+v, want exactly 2 rows", users)
	}
}
