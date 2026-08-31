package backup

import (
	"context"
	"errors"
	"testing"
)

func TestCreateAndVerifyWebUIUser(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := CreateWebUIUser(ctx, db, "alice", "hunter2", PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	perm, ok, err := VerifyWebUIUser(ctx, db, "alice", "hunter2")
	if err != nil {
		t.Fatalf("VerifyWebUIUser() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("VerifyWebUIUser() ok = false, want true for the correct password")
	}

	if perm != PermissionView {
		t.Errorf("VerifyWebUIUser() perm = %v, want %v", perm, PermissionView)
	}

	if _, ok, err := VerifyWebUIUser(ctx, db, "alice", "wrong"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() with wrong password = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := VerifyWebUIUser(ctx, db, "bob", "hunter2"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() for unknown user = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestCreateWebUIUserRejectsDuplicateUsername(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := CreateWebUIUser(ctx, db, "alice", "hunter2", PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	if err := CreateWebUIUser(ctx, db, "alice", "different", PermissionDownload); !errors.Is(err, ErrWebUIUserExists) {
		t.Errorf("CreateWebUIUser() with a duplicate username = %v, want ErrWebUIUserExists", err)
	}
}

func TestUpdateWebUIUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := CreateWebUIUser(ctx, db, "alice", "hunter2", PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	if err := UpdateWebUIUserPermissions(ctx, db, "alice", PermissionView|PermissionDownload); err != nil {
		t.Fatalf("UpdateWebUIUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := VerifyWebUIUser(ctx, db, "alice", "hunter2")
	if err != nil || !ok {
		t.Fatalf("VerifyWebUIUser() after permission update = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := PermissionView | PermissionDownload; perm != want {
		t.Errorf("VerifyWebUIUser() perm after update = %v, want %v", perm, want)
	}

	if err := UpdateWebUIUserPermissions(ctx, db, "nobody", PermissionView); !errors.Is(err, ErrWebUIUserNotFound) {
		t.Errorf("UpdateWebUIUserPermissions() for unknown user = %v, want ErrWebUIUserNotFound", err)
	}
}

func TestUpdateWebUIUserPassword(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := CreateWebUIUser(ctx, db, "alice", "hunter2", PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	if err := UpdateWebUIUserPassword(ctx, db, "alice", "newpass"); err != nil {
		t.Fatalf("UpdateWebUIUserPassword() unexpected error: %v", err)
	}

	if _, ok, err := VerifyWebUIUser(ctx, db, "alice", "hunter2"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() with the old password after a change = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, ok, err := VerifyWebUIUser(ctx, db, "alice", "newpass"); err != nil || !ok {
		t.Errorf("VerifyWebUIUser() with the new password = (ok=%v, err=%v), want (true, nil)", ok, err)
	}
}

func TestDeleteWebUIUser(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := CreateWebUIUser(ctx, db, "alice", "hunter2", PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	if err := DeleteWebUIUser(ctx, db, "alice"); err != nil {
		t.Fatalf("DeleteWebUIUser() unexpected error: %v", err)
	}

	if _, ok, err := VerifyWebUIUser(ctx, db, "alice", "hunter2"); err != nil || ok {
		t.Errorf("VerifyWebUIUser() after deletion = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := DeleteWebUIUser(ctx, db, "alice"); !errors.Is(err, ErrWebUIUserNotFound) {
		t.Errorf("DeleteWebUIUser() for an already-deleted user = %v, want ErrWebUIUserNotFound", err)
	}
}

func TestListWebUIUsersOrdersByUsername(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := CreateWebUIUser(ctx, db, "bob", "pw1", PermissionDownload); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	if err := CreateWebUIUser(ctx, db, "alice", "pw2", PermissionView); err != nil {
		t.Fatalf("CreateWebUIUser() unexpected error: %v", err)
	}

	users, err := ListWebUIUsers(ctx, db)
	if err != nil {
		t.Fatalf("ListWebUIUsers() unexpected error: %v", err)
	}

	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("ListWebUIUsers() = %+v, want [alice, bob] in that order", users)
	}

	if users[0].Permissions != PermissionView {
		t.Errorf("users[0].Permissions = %v, want %v", users[0].Permissions, PermissionView)
	}

	if users[1].Permissions != PermissionDownload {
		t.Errorf("users[1].Permissions = %v, want %v", users[1].Permissions, PermissionDownload)
	}
}
