package store

import (
	"context"
	"errors"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

func TestSaveGroupRejectsDuplicateName(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveGroup(ctx, "ops", permission.PermissionView); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "ops", permission.PermissionDownload); !errors.Is(err, ErrGroupExists) {
		t.Errorf("SaveGroup() with a duplicate name = %v, want ErrGroupExists", err)
	}
}

func TestUpdateGroupPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveGroup(ctx, "ops", permission.PermissionView); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.UpdateGroupPermissions(ctx, "ops", permission.PermissionView|permission.PermissionDownload); err != nil {
		t.Fatalf("UpdateGroupPermissions() unexpected error: %v", err)
	}

	g, ok, err := db.GetGroup(ctx, "ops")
	if err != nil || !ok {
		t.Fatalf("GetGroup() = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := permission.PermissionView | permission.PermissionDownload; g.Permissions != want {
		t.Errorf("GetGroup().Permissions = %v, want %v", g.Permissions, want)
	}

	if err := db.UpdateGroupPermissions(ctx, "nonexistent", permission.PermissionView); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("UpdateGroupPermissions() for unknown group = %v, want ErrGroupNotFound", err)
	}
}

func TestListGroupsOrdersByName(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveGroup(ctx, "zeta", permission.PermissionView); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "alpha", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	groups, err := db.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups() unexpected error: %v", err)
	}

	if len(groups) != 2 || groups[0].Name != "alpha" || groups[1].Name != "zeta" {
		t.Fatalf("ListGroups() = %+v, want [alpha, zeta] in that order", groups)
	}
}

func TestDeleteGroupClearsMemberships(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "ops", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SetUserGroups(ctx, "alice", []string{"ops"}); err != nil {
		t.Fatalf("SetUserGroups() unexpected error: %v", err)
	}

	if err := db.DeleteGroup(ctx, "ops"); err != nil {
		t.Fatalf("DeleteGroup() unexpected error: %v", err)
	}

	user := mustGetUser(ctx, t, db, "alice")

	if len(user.Groups) != 0 {
		t.Errorf("user.Groups after deleting its only group = %v, want empty", user.Groups)
	}

	if user.EffectivePermissions != permission.PermissionView {
		t.Errorf("user.EffectivePermissions after deleting its only group = %v, want %v", user.EffectivePermissions, permission.PermissionView)
	}

	if err := db.DeleteGroup(ctx, "ops"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("DeleteGroup() for an already-deleted group = %v, want ErrGroupNotFound", err)
	}
}

func TestSetUserGroupsValidatesUserAndGroupExist(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SetUserGroups(ctx, "nobody", []string{}); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetUserGroups() for unknown user = %v, want ErrUserNotFound", err)
	}

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SetUserGroups(ctx, "alice", []string{"nonexistent"}); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("SetUserGroups() with an unknown group = %v, want ErrGroupNotFound", err)
	}
}

func TestSetUserGroupsReplacesMembershipWholesale(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", 0); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "readers", permission.PermissionView); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "downloaders", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SetUserGroups(ctx, "alice", []string{"readers", "downloaders"}); err != nil {
		t.Fatalf("SetUserGroups() unexpected error: %v", err)
	}

	user := mustGetUser(ctx, t, db, "alice")

	if want := permission.PermissionView | permission.PermissionDownload; user.EffectivePermissions != want {
		t.Errorf("EffectivePermissions after joining two groups = %v, want %v", user.EffectivePermissions, want)
	}

	// Replacing with a smaller set drops the membership no longer listed.
	if err := db.SetUserGroups(ctx, "alice", []string{"readers"}); err != nil {
		t.Fatalf("SetUserGroups() unexpected error: %v", err)
	}

	user = mustGetUser(ctx, t, db, "alice")

	if len(user.Groups) != 1 || user.Groups[0] != "readers" {
		t.Errorf("Groups after replacing membership = %v, want [readers]", user.Groups)
	}

	if user.EffectivePermissions != permission.PermissionView {
		t.Errorf("EffectivePermissions after replacing membership = %v, want %v", user.EffectivePermissions, permission.PermissionView)
	}
}

func TestVerifyUserIncludesGroupPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if err := db.SaveUser(ctx, "alice", "hunter2", "", permission.PermissionView); err != nil {
		t.Fatalf("SaveUser() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "downloaders", permission.PermissionDownload); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SetUserGroups(ctx, "alice", []string{"downloaders"}); err != nil {
		t.Fatalf("SetUserGroups() unexpected error: %v", err)
	}

	perm, ok, err := db.VerifyUser(ctx, "alice", "hunter2")
	if err != nil || !ok {
		t.Fatalf("VerifyUser() = (ok=%v, err=%v), want (true, nil)", ok, err)
	}

	if want := permission.PermissionView | permission.PermissionDownload; perm != want {
		t.Errorf("VerifyUser() perm = %v, want %v (own + group)", perm, want)
	}
}

func TestGetOrProvisionOIDCUserIncludesGroupPermissions(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	if _, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView); err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	if err := db.SaveGroup(ctx, "admins", permission.PermissionAdmin); err != nil {
		t.Fatalf("SaveGroup() unexpected error: %v", err)
	}

	if err := db.SetUserGroups(ctx, "alice@example.com", []string{"admins"}); err != nil {
		t.Fatalf("SetUserGroups() unexpected error: %v", err)
	}

	perm, err := db.GetOrProvisionOIDCUser(ctx, "alice@example.com", permission.PermissionView)
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser() unexpected error: %v", err)
	}

	if want := permission.PermissionView | permission.PermissionAdmin; perm != want {
		t.Errorf("GetOrProvisionOIDCUser() perm on later login = %v, want %v (own + group)", perm, want)
	}
}
