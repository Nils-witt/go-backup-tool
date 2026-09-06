package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// groupModel is groups: a named, admin-managed bundle of permissions (see
// the permission package) that a users.go row can join via userGroupModel,
// picking up every bit the group grants on top of whatever it's granted
// directly. Exists so an admin can grant/revoke a whole set of permissions
// across many accounts at once by editing one group, rather than editing
// each account's own Permissions bitmask.
type groupModel struct {
	Name        string    `gorm:"column:name;primaryKey"`
	Permissions int       `gorm:"column:permissions;not null;default:0"`
	CreatedAt   time.Time `gorm:"column:created_at;not null"`
}

func (groupModel) TableName() string { return "groups" }

// userGroupModel is user_groups: one (username, group_name) membership row,
// the many-to-many join between users and groups. Deleted alongside its
// group (see DeleteGroup) or its user (left to DeleteUser's own cascade via
// SetUserGroups not being involved there — see the doc on DeleteUser) and
// wholesale replaced on every SetUserGroups call.
type userGroupModel struct {
	Username  string `gorm:"column:username;primaryKey"`
	GroupName string `gorm:"column:group_name;primaryKey;index"`
}

func (userGroupModel) TableName() string { return "user_groups" }

// Group is one groups row, as returned by ListGroups/GetGroup.
type Group struct {
	Name        string
	Permissions permission.Permission
	CreatedAt   time.Time
}

// ErrGroupExists is returned by SaveGroup when name is already taken.
var ErrGroupExists = errors.New("group already exists")

// ErrGroupNotFound is returned by UpdateGroupPermissions/DeleteGroup/
// SetUserGroups when a named group doesn't exist.
var ErrGroupNotFound = errors.New("group not found")

// SaveGroup adds a new group granting perm. Returns ErrGroupExists if name
// is already taken.
func (s *Store) SaveGroup(ctx context.Context, name string, perm permission.Permission) error {
	m := groupModel{Name: name, Permissions: int(perm), CreatedAt: time.Now().UTC()}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		if isUniqueConstraintErrOn(err, "groups.name") {
			return ErrGroupExists
		}

		return fmt.Errorf("creating group %q: %w", name, err)
	}

	return nil
}

// checkGroupRowsAffected reports ErrGroupNotFound if result (from an
// UPDATE/DELETE keyed by group name) touched no rows, meaning name didn't
// exist.
func checkGroupRowsAffected(result *gorm.DB, verb, name string) error {
	if result.Error != nil {
		return fmt.Errorf("%s group %q: %w", verb, name, result.Error)
	}

	if result.RowsAffected == 0 {
		return ErrGroupNotFound
	}

	return nil
}

// UpdateGroupPermissions changes name's granted permissions to perm.
// Returns ErrGroupNotFound if name doesn't exist.
func (s *Store) UpdateGroupPermissions(ctx context.Context, name string, perm permission.Permission) error {
	result := s.db.WithContext(ctx).Model(&groupModel{}).Where("name = ?", name).Update("permissions", int(perm))

	return checkGroupRowsAffected(result, "updating", name)
}

// DeleteGroup removes name and every membership row referencing it.
// Returns ErrGroupNotFound if it doesn't exist. Runs in a transaction since
// it touches two tables and the db must not end up with a dangling
// membership row if the process dies mid-way.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Where("name = ?", name).Delete(&groupModel{})
		if err := checkGroupRowsAffected(result, "deleting", name); err != nil {
			return err
		}

		if err := tx.Where("group_name = ?", name).Delete(&userGroupModel{}).Error; err != nil {
			return fmt.Errorf("clearing memberships for deleted group %q: %w", name, err)
		}

		return nil
	})
}

// toGroup converts a groupModel row into the exported Group shape.
func toGroup(m groupModel) Group {
	return Group{Name: m.Name, Permissions: permission.Permission(m.Permissions), CreatedAt: m.CreatedAt}
}

// ListGroups returns every group, in name order, for the "Users" admin
// section's group listing.
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	var rows []groupModel

	if err := s.db.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}

	groups := make([]Group, len(rows))
	for i, m := range rows {
		groups[i] = toGroup(m)
	}

	return groups, nil
}

// GetGroup returns the named group, reporting ok=false if name doesn't
// exist.
func (s *Store) GetGroup(ctx context.Context, name string) (Group, bool, error) {
	var m groupModel

	err := s.db.WithContext(ctx).Where("name = ?", name).Take(&m).Error

	switch {
	case isRecordNotFound(err):
		return Group{}, false, nil
	case err != nil:
		return Group{}, false, fmt.Errorf("looking up group %q: %w", name, err)
	default:
		return toGroup(m), true, nil
	}
}

// dedupStrings returns ss with duplicates removed, preserving first-seen
// order — SetUserGroups' defense against a caller submitting the same group
// name twice, which would otherwise trip user_groups' (username, group_name)
// primary key on the second insert.
func dedupStrings(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))

	for _, s := range ss {
		if _, ok := seen[s]; ok {
			continue
		}

		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}

// SetUserGroups replaces username's group memberships wholesale with
// groupNames — the same full-replace shape as UpdateUserPermissions, since
// the "Users" admin section always resubmits a row's whole membership set
// rather than adding/removing one at a time. Returns ErrUserNotFound if
// username doesn't exist, or ErrGroupNotFound if any name in groupNames
// doesn't name an existing group. Runs in a transaction: membership rows are
// cleared and reinserted, and a partial replace must never be observable.
func (s *Store) SetUserGroups(ctx context.Context, username string, groupNames []string) error {
	groupNames = dedupStrings(groupNames)

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var userExists bool
		if err := tx.Raw(`SELECT EXISTS(SELECT 1 FROM users WHERE username = ?)`, username).Scan(&userExists).Error; err != nil {
			return fmt.Errorf("checking user %q exists: %w", username, err)
		}

		if !userExists {
			return ErrUserNotFound
		}

		for _, name := range groupNames {
			var groupExists bool
			if err := tx.Raw(`SELECT EXISTS(SELECT 1 FROM groups WHERE name = ?)`, name).Scan(&groupExists).Error; err != nil {
				return fmt.Errorf("checking group %q exists: %w", name, err)
			}

			if !groupExists {
				return fmt.Errorf("%w: %q", ErrGroupNotFound, name)
			}
		}

		if err := tx.Where("username = ?", username).Delete(&userGroupModel{}).Error; err != nil {
			return fmt.Errorf("clearing groups for user %q: %w", username, err)
		}

		for _, name := range groupNames {
			if err := tx.Create(&userGroupModel{Username: username, GroupName: name}).Error; err != nil {
				return fmt.Errorf("adding user %q to group %q: %w", username, name, err)
			}
		}

		return nil
	})
}

// userGroupInfo is loadUserGroups' per-username result: the names of every
// group a user belongs to, and those groups' permissions already OR'd
// together, so callers needing only the effective bitmask (VerifyUser,
// GetOrProvisionOIDCUser) don't have to walk Names themselves.
type userGroupInfo struct {
	Names []string
	Perm  permission.Permission
}

// loadUserGroups returns each of usernames' group memberships, joined with
// each group's own permissions. A username with no memberships is simply
// absent from the result rather than present with a zero-value entry, so
// callers should treat a missing key as "no groups" (the zero userGroupInfo
// already means that).
func (s *Store) loadUserGroups(ctx context.Context, usernames []string) (map[string]userGroupInfo, error) {
	result := make(map[string]userGroupInfo)

	if len(usernames) == 0 {
		return result, nil
	}

	type row struct {
		Username    string
		GroupName   string
		Permissions int
	}

	var rows []row

	err := s.db.WithContext(ctx).
		Table("user_groups").
		Select("user_groups.username AS username, groups.name AS group_name, groups.permissions AS permissions").
		Joins("JOIN groups ON groups.name = user_groups.group_name").
		Where("user_groups.username IN ?", usernames).
		Order("groups.name").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("loading user groups: %w", err)
	}

	for _, r := range rows {
		info := result[r.Username]
		info.Names = append(info.Names, r.GroupName)
		info.Perm |= permission.Permission(r.Permissions)
		result[r.Username] = info
	}

	return result, nil
}

// groupPermissionsFor returns the union of every permission granted to
// username through its group memberships (0 if it belongs to none) — the
// contribution SetUserGroups/its groups add on top of a row's own
// Permissions to form the effective permission a login session carries (see
// VerifyUser/GetOrProvisionOIDCUser).
func (s *Store) groupPermissionsFor(ctx context.Context, username string) (permission.Permission, error) {
	info, err := s.loadUserGroups(ctx, []string{username})
	if err != nil {
		return 0, err
	}

	return info[username].Perm, nil
}
