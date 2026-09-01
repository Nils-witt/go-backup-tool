package store

import (
	"context"
	"errors"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

func TestSaveAndGetOIDCUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if _, ok, err := db.GetOIDCUserPermissions(ctx, "alice@example.com"); err != nil || ok {
		t.Fatalf("GetOIDCUserPermissions() before any override = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := db.SaveOIDCUserPermissions(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveOIDCUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := db.GetOIDCUserPermissions(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("GetOIDCUserPermissions() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("GetOIDCUserPermissions() ok = false, want true after SaveOIDCUserPermissions")
	}

	if perm != permission.PermissionView {
		t.Errorf("GetOIDCUserPermissions() perm = %v, want %v", perm, permission.PermissionView)
	}
}

func TestSaveOIDCUserPermissionsUpsertsExisting(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveOIDCUserPermissions(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveOIDCUserPermissions() unexpected error: %v", err)
	}

	if err := db.SaveOIDCUserPermissions(ctx, "alice@example.com", permission.PermissionView|permission.PermissionDownload); err != nil {
		t.Fatalf("SaveOIDCUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := db.GetOIDCUserPermissions(ctx, "alice@example.com")
	if err != nil || !ok {
		t.Fatalf("GetOIDCUserPermissions() after re-set = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := permission.PermissionView | permission.PermissionDownload; perm != want {
		t.Errorf("GetOIDCUserPermissions() perm after re-set = %v, want %v", perm, want)
	}
}

func TestDeleteOIDCUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveOIDCUserPermissions(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveOIDCUserPermissions() unexpected error: %v", err)
	}

	if err := db.DeleteOIDCUserPermissions(ctx, "alice@example.com"); err != nil {
		t.Fatalf("DeleteOIDCUserPermissions() unexpected error: %v", err)
	}

	if _, ok, err := db.GetOIDCUserPermissions(ctx, "alice@example.com"); err != nil || ok {
		t.Errorf("GetOIDCUserPermissions() after deletion = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := db.DeleteOIDCUserPermissions(ctx, "alice@example.com"); !errors.Is(err, ErrOIDCUserPermissionsNotFound) {
		t.Errorf("DeleteOIDCUserPermissions() for an already-deleted override = %v, want ErrOIDCUserPermissionsNotFound", err)
	}
}

func TestListOIDCUserPermissionsOrdersByIdentity(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveOIDCUserPermissions(ctx, "bob@example.com", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveOIDCUserPermissions() unexpected error: %v", err)
	}

	if err := db.SaveOIDCUserPermissions(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("SaveOIDCUserPermissions() unexpected error: %v", err)
	}

	users, err := db.ListOIDCUserPermissions(ctx)
	if err != nil {
		t.Fatalf("ListOIDCUserPermissions() unexpected error: %v", err)
	}

	if len(users) != 2 || users[0].Identity != "alice@example.com" || users[1].Identity != "bob@example.com" {
		t.Fatalf("ListOIDCUserPermissions() = %+v, want [alice@example.com, bob@example.com] in that order", users)
	}

	if users[0].Permissions != permission.PermissionView {
		t.Errorf("users[0].Permissions = %v, want %v", users[0].Permissions, permission.PermissionView)
	}

	if users[1].Permissions != permission.PermissionDownload {
		t.Errorf("users[1].Permissions = %v, want %v", users[1].Permissions, permission.PermissionDownload)
	}
}
