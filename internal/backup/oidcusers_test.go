package backup

import (
	"context"
	"errors"
	"testing"
)

func TestSetAndGetOIDCUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if _, ok, err := OIDCUserPermissions(ctx, db, "alice@example.com"); err != nil || ok {
		t.Fatalf("OIDCUserPermissions() before any override = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := SetOIDCUserPermissions(ctx, db, "alice@example.com", PermissionView); err != nil {
		t.Fatalf("SetOIDCUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := OIDCUserPermissions(ctx, db, "alice@example.com")
	if err != nil {
		t.Fatalf("OIDCUserPermissions() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("OIDCUserPermissions() ok = false, want true after SetOIDCUserPermissions")
	}

	if perm != PermissionView {
		t.Errorf("OIDCUserPermissions() perm = %v, want %v", perm, PermissionView)
	}
}

func TestSetOIDCUserPermissionsUpsertsExisting(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := SetOIDCUserPermissions(ctx, db, "alice@example.com", PermissionView); err != nil {
		t.Fatalf("SetOIDCUserPermissions() unexpected error: %v", err)
	}

	if err := SetOIDCUserPermissions(ctx, db, "alice@example.com", PermissionView|PermissionDownload); err != nil {
		t.Fatalf("SetOIDCUserPermissions() unexpected error: %v", err)
	}

	perm, ok, err := OIDCUserPermissions(ctx, db, "alice@example.com")
	if err != nil || !ok {
		t.Fatalf("OIDCUserPermissions() after re-set = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := PermissionView | PermissionDownload; perm != want {
		t.Errorf("OIDCUserPermissions() perm after re-set = %v, want %v", perm, want)
	}
}

func TestDeleteOIDCUserPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := SetOIDCUserPermissions(ctx, db, "alice@example.com", PermissionView); err != nil {
		t.Fatalf("SetOIDCUserPermissions() unexpected error: %v", err)
	}

	if err := DeleteOIDCUserPermissions(ctx, db, "alice@example.com"); err != nil {
		t.Fatalf("DeleteOIDCUserPermissions() unexpected error: %v", err)
	}

	if _, ok, err := OIDCUserPermissions(ctx, db, "alice@example.com"); err != nil || ok {
		t.Errorf("OIDCUserPermissions() after deletion = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := DeleteOIDCUserPermissions(ctx, db, "alice@example.com"); !errors.Is(err, ErrOIDCUserPermissionsNotFound) {
		t.Errorf("DeleteOIDCUserPermissions() for an already-deleted override = %v, want ErrOIDCUserPermissionsNotFound", err)
	}
}

func TestListOIDCUserPermissionsOrdersByIdentity(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := context.Background()

	if err := SetOIDCUserPermissions(ctx, db, "bob@example.com", PermissionDownload); err != nil {
		t.Fatalf("SetOIDCUserPermissions() unexpected error: %v", err)
	}

	if err := SetOIDCUserPermissions(ctx, db, "alice@example.com", PermissionView); err != nil {
		t.Fatalf("SetOIDCUserPermissions() unexpected error: %v", err)
	}

	users, err := ListOIDCUserPermissions(ctx, db)
	if err != nil {
		t.Fatalf("ListOIDCUserPermissions() unexpected error: %v", err)
	}

	if len(users) != 2 || users[0].Identity != "alice@example.com" || users[1].Identity != "bob@example.com" {
		t.Fatalf("ListOIDCUserPermissions() = %+v, want [alice@example.com, bob@example.com] in that order", users)
	}

	if users[0].Permissions != PermissionView {
		t.Errorf("users[0].Permissions = %v, want %v", users[0].Permissions, PermissionView)
	}

	if users[1].Permissions != PermissionDownload {
		t.Errorf("users[1].Permissions = %v, want %v", users[1].Permissions, PermissionDownload)
	}
}
