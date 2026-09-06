package store

import (
	"context"
	"database/sql"
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
// each account's own Permissions bitmask. OIDCGroupName, when set, names the
// group/role this group is mapped to in an OIDC provider's "groups" claim
// (see SyncOIDCGroups in oidc.go's handleOIDCCallback): on every SSO login,
// membership in every group with a non-empty OIDCGroupName is resynced to
// match the provider's claim, while groups with no mapping stay purely
// admin-managed. UNIQUE so two groups can't both claim the same provider
// group, which would make the mapping ambiguous.
type groupModel struct {
	Name          string         `gorm:"column:name;primaryKey"`
	Permissions   int            `gorm:"column:permissions;not null;default:0"`
	OIDCGroupName sql.NullString `gorm:"column:oidc_group_name;uniqueIndex"`
	CreatedAt     time.Time      `gorm:"column:created_at;not null"`
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

// Group is one groups row, as returned by ListGroups/GetGroup. OIDCGroupName
// is "" if this group has no OIDC provider mapping (see groupModel).
type Group struct {
	Name          string
	Permissions   permission.Permission
	OIDCGroupName string
	CreatedAt     time.Time
}

// ErrGroupExists is returned by SaveGroup when name is already taken.
var ErrGroupExists = errors.New("group already exists")

// ErrGroupNotFound is returned by UpdateGroupPermissions/SetGroupOIDCGroupName/
// DeleteGroup/SetUserGroups when a named group doesn't exist.
var ErrGroupNotFound = errors.New("group not found")

// ErrOIDCGroupNameTaken is returned by SaveGroup/SetGroupOIDCGroupName when
// the given OIDC group name is already mapped to a different group.
var ErrOIDCGroupNameTaken = errors.New("oidc group name is already mapped to another group")

// SaveGroup adds a new group granting perm, mapped to oidcGroupName (see
// groupModel) if non-empty. Returns ErrGroupExists if name is already taken,
// or ErrOIDCGroupNameTaken if oidcGroupName already maps another group.
func (s *Store) SaveGroup(ctx context.Context, name string, perm permission.Permission, oidcGroupName string) error {
	m := groupModel{Name: name, Permissions: int(perm), OIDCGroupName: nullString(oidcGroupName), CreatedAt: time.Now().UTC()}

	if err := s.db.WithContext(ctx).Create(&m).Error; err != nil {
		switch {
		case isUniqueConstraintErrOn(err, "groups.name"):
			return ErrGroupExists
		case isUniqueConstraintErrOn(err, "groups.oidc_group_name"):
			return ErrOIDCGroupNameTaken
		default:
			return fmt.Errorf("creating group %q: %w", name, err)
		}
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

// SetGroupOIDCGroupName links name's group to oidcGroupName, or clears any
// existing mapping when oidcGroupName is "" (see groupModel/SyncOIDCGroups).
// Returns ErrGroupNotFound if name doesn't exist, or ErrOIDCGroupNameTaken if
// oidcGroupName already maps a different group.
func (s *Store) SetGroupOIDCGroupName(ctx context.Context, name, oidcGroupName string) error {
	result := s.db.WithContext(ctx).Model(&groupModel{}).Where("name = ?", name).Update("oidc_group_name", nullString(oidcGroupName))
	if result.Error != nil {
		if isUniqueConstraintErrOn(result.Error, "groups.oidc_group_name") {
			return ErrOIDCGroupNameTaken
		}

		return fmt.Errorf("mapping group %q to oidc group %q: %w", name, oidcGroupName, result.Error)
	}

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
	return Group{
		Name:          m.Name,
		Permissions:   permission.Permission(m.Permissions),
		OIDCGroupName: m.OIDCGroupName.String,
		CreatedAt:     m.CreatedAt,
	}
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

// SyncOIDCGroups reconciles the account linked to oidcUsername's (see
// GetOrProvisionOIDCUser) membership in every OIDC-managed group — one with
// a non-empty OIDCGroupName (see groupModel) — against oidcGroups, the raw
// group names/IDs an OIDC provider's "groups" claim reported for this login:
// it joins every managed group whose OIDCGroupName appears in oidcGroups and
// leaves every other managed group, while membership in any unmanaged group
// (no OIDCGroupName set — assigned by hand through the "Users" admin section
// instead) is left untouched either way. Called on every SSO login (see
// handleOIDCCallback in oidc.go), so a change to a person's groups on the
// provider's side takes effect the next time they log in. Returns the
// account's resulting effective permission (its own Permissions OR'd with
// every group it now belongs to, managed or not), or ErrUserNotFound if
// oidcUsername doesn't name a provisioned row. Runs in a transaction:
// membership rows are cleared and reinserted, and a partial resync must
// never be observable.
func (s *Store) SyncOIDCGroups(ctx context.Context, oidcUsername string, oidcGroups []string) (permission.Permission, error) {
	var perm permission.Permission

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user userModel
		if err := tx.Select("username", "permissions").Where("oidc_username = ?", oidcUsername).Take(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}

			return fmt.Errorf("looking up oidc user %q: %w", oidcUsername, err)
		}

		var allGroups []groupModel
		if err := tx.Find(&allGroups).Error; err != nil {
			return fmt.Errorf("loading groups: %w", err)
		}

		nameForOIDCGroup, managedNames, permForName := indexOIDCGroups(allGroups)

		var current []userGroupModel
		if err := tx.Where("username = ?", user.Username).Find(&current).Error; err != nil {
			return fmt.Errorf("loading groups for user %q: %w", user.Username, err)
		}

		final := resyncedGroupNames(current, oidcGroups, nameForOIDCGroup, managedNames)

		if err := replaceUserGroupRows(tx, user.Username, final); err != nil {
			return err
		}

		var groupPerm permission.Permission
		for _, name := range final {
			groupPerm |= permForName[name]
		}

		perm = permission.Permission(user.Permissions) | groupPerm

		return nil
	})
	if err != nil {
		return 0, err
	}

	return perm, nil
}

// indexOIDCGroups indexes allGroups for SyncOIDCGroups: nameForOIDCGroup maps
// each OIDC-managed group's OIDCGroupName to its local Name; managedNames is
// the set of local names that are OIDC-managed, regardless of whether this
// login's own oidcGroups happens to include them; permForName maps every
// group's Name to its granted permission, managed or not.
func indexOIDCGroups(allGroups []groupModel) (nameForOIDCGroup map[string]string, managedNames map[string]struct{}, permForName map[string]permission.Permission) {
	nameForOIDCGroup = make(map[string]string, len(allGroups))
	managedNames = make(map[string]struct{}, len(allGroups))
	permForName = make(map[string]permission.Permission, len(allGroups))

	for _, g := range allGroups {
		permForName[g.Name] = permission.Permission(g.Permissions)

		if g.OIDCGroupName.Valid {
			nameForOIDCGroup[g.OIDCGroupName.String] = g.Name
			managedNames[g.Name] = struct{}{}
		}
	}

	return nameForOIDCGroup, managedNames, permForName
}

// resyncedGroupNames returns a user's post-sync group membership (see
// SyncOIDCGroups), given its current membership rows, the OIDC provider's
// own "groups" claim for this login, and indexOIDCGroups' lookups: every
// current membership in an unmanaged group survives untouched, alongside
// every managed group whose OIDCGroupName appears in oidcGroups.
func resyncedGroupNames(current []userGroupModel, oidcGroups []string, nameForOIDCGroup map[string]string, managedNames map[string]struct{}) []string {
	final := make([]string, 0, len(current)+len(oidcGroups))

	for _, m := range current {
		if _, managed := managedNames[m.GroupName]; !managed {
			final = append(final, m.GroupName)
		}
	}

	for _, oidcGroup := range oidcGroups {
		if name, ok := nameForOIDCGroup[oidcGroup]; ok {
			final = append(final, name)
		}
	}

	return dedupStrings(final)
}

// replaceUserGroupRows wholesale-replaces username's user_groups rows with
// names, within tx — the write half of SyncOIDCGroups, factored out since
// its names are already known to name existing groups (see
// indexOIDCGroups), unlike SetUserGroups' own caller-supplied names, which
// still need that existence check.
func replaceUserGroupRows(tx *gorm.DB, username string, names []string) error {
	if err := tx.Where("username = ?", username).Delete(&userGroupModel{}).Error; err != nil {
		return fmt.Errorf("clearing groups for user %q: %w", username, err)
	}

	for _, name := range names {
		if err := tx.Create(&userGroupModel{Username: username, GroupName: name}).Error; err != nil {
			return fmt.Errorf("adding user %q to group %q: %w", username, name, err)
		}
	}

	return nil
}
