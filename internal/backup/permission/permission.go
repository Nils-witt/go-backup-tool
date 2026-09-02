// Package permission defines the web UI dashboard's session permission
// bitmask (view/download/admin/login-log/download-log) and its config-file
// and admin-API name parsing.
package permission

import (
	"fmt"
	"strings"
)

// Permission is a dashboard session's granted capabilities, on top of
// having merely authenticated: what a logged-in user is allowed to see and
// do. Stored as a bitmask — on disk in users.permissions (see users.go) for
// an account managed through the web UI's "Users" admin section, and
// embedded directly in that session's own bearer token at login time (see
// sessionStore.create in webui.go) so every request can check it without a
// server-side lookup, the same way the rest of that token's claims work.
type Permission int

const (
	// PermissionView lets a session see the dashboard's job/target/receiver
	// status, file listings, and application log views — everything
	// StartWebUI serves under its api(...) wrapper except minting a
	// download ticket, viewing the login history
	// (PermissionViewLoginLog), and viewing the download history
	// (PermissionViewDownloadLog) — those three are granted separately.
	PermissionView Permission = 1 << iota

	// PermissionDownload lets a session mint a download ticket and pull a
	// file's actual content (see handleMintDownloadTicket/handleDownloadFile
	// in webui.go). It implies PermissionView (see CanView) — there'd be no
	// way to discover a file to download without also being able to view
	// the file listing it comes from.
	PermissionDownload

	// PermissionAdmin lets a session manage the web UI's "Users" admin
	// section itself — create/update/delete other web UI accounts and OIDC
	// permission overrides, and issue long-lived API tokens (see
	// requireAdmin in webui.go) — access that used to require being the
	// single config-file admin (webui.username/webui.password), now
	// assignable to a "Users" admin-managed account or an OIDC identity
	// instead. It implies PermissionView, PermissionDownload,
	// PermissionViewLoginLog, and PermissionViewDownloadLog (see
	// CanView/CanDownload/CanViewLoginLog/CanViewDownloadLog) — there'd be
	// no way to administer the dashboard's users without also being able to
	// use the dashboard itself.
	PermissionAdmin

	// PermissionViewLoginLog lets a session see the dashboard's login
	// history (see handleLoginEvents in webui.go) — who logged in, when,
	// and from where. Granted independently of PermissionView, so a
	// session holding only PermissionView can't see it; implied by
	// PermissionAdmin (see CanViewLoginLog).
	PermissionViewLoginLog

	// PermissionViewDownloadLog lets a session see the dashboard's file
	// download history (see handleDownloadEvents in webui.go) — who
	// downloaded which file, and when. Granted independently of
	// PermissionView/PermissionDownload, so a session holding only those
	// can't see it; implied by PermissionAdmin (see CanViewDownloadLog).
	PermissionViewDownloadLog
)

// permissionNames maps each individual bit to its wire/config name, in
// canonical order. Shared by Names (bitmask -> names, for the "Users" admin
// API and /api/session) and ParsePermissions (names -> bitmask, for the
// config file's webui.oidc.default-permissions: and the admin API's
// request bodies).
var permissionNames = []struct {
	bit  Permission
	name string
}{
	{PermissionView, "view"},
	{PermissionDownload, "download"},
	{PermissionAdmin, "admin"},
	{PermissionViewLoginLog, "login-log"},
	{PermissionViewDownloadLog, "download-log"},
}

// CanView reports whether p includes the ability to view dashboard data —
// either granted directly, or implied by PermissionDownload or
// PermissionAdmin.
func (p Permission) CanView() bool {
	return p&(PermissionView|PermissionDownload|PermissionAdmin) != 0
}

// CanDownload reports whether p includes the ability to download files —
// either granted directly, or implied by PermissionAdmin.
func (p Permission) CanDownload() bool {
	return p&(PermissionDownload|PermissionAdmin) != 0
}

// CanAdmin reports whether p includes the ability to manage the web UI's
// "Users" admin section (see PermissionAdmin).
func (p Permission) CanAdmin() bool {
	return p&PermissionAdmin != 0
}

// CanViewLoginLog reports whether p includes the ability to see the
// dashboard's login history — either granted directly, or implied by
// PermissionAdmin.
func (p Permission) CanViewLoginLog() bool {
	return p&(PermissionViewLoginLog|PermissionAdmin) != 0
}

// CanViewDownloadLog reports whether p includes the ability to see the
// dashboard's file download history — either granted directly, or implied
// by PermissionAdmin.
func (p Permission) CanViewDownloadLog() bool {
	return p&(PermissionViewDownloadLog|PermissionAdmin) != 0
}

// Names returns the individually-granted permission names in p, in
// canonical order. A permission only implied by another (e.g.
// PermissionView, implied by PermissionDownload — see CanView) isn't
// included unless it's also granted directly; callers that care about the
// effective, implied check should use CanView/CanDownload instead.
func (p Permission) Names() []string {
	var names []string

	for _, e := range permissionNames {
		if p&e.bit != 0 {
			names = append(names, e.name)
		}
	}

	return names
}

// ParsePermissions parses names (e.g. from the config file's
// webui.oidc.default-permissions: list, or the web UI's user-management
// API) into a Permission bitmask, rejecting any name not in permissionNames.
func ParsePermissions(names []string) (Permission, error) {
	var p Permission

	for _, name := range names {
		matched := false

		for _, e := range permissionNames {
			if e.name == name {
				p |= e.bit
				matched = true

				break
			}
		}

		if !matched {
			want := make([]string, len(permissionNames))
			for i, e := range permissionNames {
				want[i] = fmt.Sprintf("%q", e.name)
			}

			return 0, fmt.Errorf("unknown permission %q (want one of %s)", name, strings.Join(want, ", "))
		}
	}

	return p, nil
}
