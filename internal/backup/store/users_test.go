package store

import (
	"context"
	"errors"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

func TestSaveAndVerifyWebUIUser(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveWebUIUser(ctx, "alice", "hunter2", permission.PermissionView); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	perm, ok, err := db.VerifyWebUIUser(ctx, "alice", "hunter2")
	if err != nil {
		t.Fatalf("VerifyWebUIUser() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("VerifyWebUIUser() ok = false, want true for the correct password")
	}

	if perm != permission.PermissionView {
		t.Errorf("VerifyWebUIUser() perm = %v, want %v", perm, permission.PermissionView)
	}

	if _, ok, err := db.VerifyWebUIUser(ctx, "alice", "wrong"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() with wrong password = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := db.VerifyWebUIUser(ctx, "bob", "hunter2"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() for unknown user = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestSaveWebUIUserRejectsDuplicateUsername(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveWebUIUser(ctx, "alice", "hunter2", permission.PermissionView); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	if err := db.SaveWebUIUser(ctx, "alice", "different", permission.PermissionDownload); !errors.Is(err, ErrWebUIUserExists) {
		t.Errorf("SaveWebUIUser() with a duplicate username = %v, want ErrWebUIUserExists", err)
	}
}

func TestUpdateWebUIUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveWebUIUser(ctx, "alice", "hunter2", permission.PermissionView); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	if err := db.UpdateWebUIUserPermissions(ctx, "alice", permission.PermissionView|permission.PermissionDownload); err != nil {
		t.Fatalf("UpdateWebUIUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := db.VerifyWebUIUser(ctx, "alice", "hunter2")
	if err != nil || !ok {
		t.Fatalf("VerifyWebUIUser() after permission update = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := permission.PermissionView | permission.PermissionDownload; perm != want {
		t.Errorf("VerifyWebUIUser() perm after update = %v, want %v", perm, want)
	}

	if err := db.UpdateWebUIUserPermissions(ctx, "nobody", permission.PermissionView); !errors.Is(err, ErrWebUIUserNotFound) {
		t.Errorf("UpdateWebUIUserPermissions() for unknown user = %v, want ErrWebUIUserNotFound", err)
	}
}

func TestUpdateWebUIUserPassword(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveWebUIUser(ctx, "alice", "hunter2", permission.PermissionView); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	if err := db.UpdateWebUIUserPassword(ctx, "alice", "newpass"); err != nil {
		t.Fatalf("UpdateWebUIUserPassword() unexpected error: %v", err)
	}

	if _, ok, err := db.VerifyWebUIUser(ctx, "alice", "hunter2"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() with the old password after a change = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := db.VerifyWebUIUser(ctx, "alice", "newpass"); err != nil || !ok {
		t.Errorf("VerifyWebUIUser() with the new password = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
}

func TestDeleteWebUIUser(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveWebUIUser(ctx, "alice", "hunter2", permission.PermissionView); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	if err := db.DeleteWebUIUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteWebUIUser() unexpected error: %v", err)
	}

	if _, ok, err := db.VerifyWebUIUser(ctx, "alice", "hunter2"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() after deletion = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := db.DeleteWebUIUser(ctx, "alice"); !errors.Is(err, ErrWebUIUserNotFound) {
		t.Errorf("DeleteWebUIUser() for an already-deleted user = %v, want ErrWebUIUserNotFound", err)
	}
}

func TestListWebUIUsersOrdersByUsername(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveWebUIUser(ctx, "bob", "pw1", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	if err := db.SaveWebUIUser(ctx, "alice", "pw2", permission.PermissionView); err != nil {
		t.Fatalf("SaveWebUIUser() unexpected error: %v", err)
	}

	users, err := db.ListWebUIUsers(ctx)
	if err != nil {
		t.Fatalf("ListWebUIUsers() unexpected error: %v", err)
	}

	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("ListWebUIUsers() = %+v, want [alice, bob] in that order", users)
	}

	if users[0].Permissions != permission.PermissionView {
		t.Errorf("users[0].Permissions = %v, want %v", users[0].Permissions, permission.PermissionView)
	}

	if users[1].Permissions != permission.PermissionDownload {
		t.Errorf("users[1].Permissions = %v, want %v", users[1].Permissions, permission.PermissionDownload)
	}
}
