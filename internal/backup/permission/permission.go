// Package permission defines the web UI dashboard's permission bitmask
// (view/download/admin/login-log/download-log/job-run-log/target-run-log/
// receiver-log/audit-log) and its config-file name parsing.
package permission

import (
	"fmt"
	"strings"
)

// Permission is a signed-in user's granted capabilities, on top of having
// merely authenticated: what they're allowed to see and do. Never stored:
// it's worked out on every request from the SSO access token, as the config
// file's webui.oidc.default-permissions ORed with whatever the token's
// groups are mapped to in webui.oidc.group-permissions (see
// internal/backup/webui/auth.go).
type Permission int

const (
	// PermissionView lets a session see the dashboard's job/target/receiver
	// status, file listings, and application log views — everything
	// StartWebUI serves under its api(...) wrapper except minting a
	// download ticket, viewing the login history
	// (PermissionViewLoginLog), viewing the download history
	// (PermissionViewDownloadLog), viewing the job run log
	// (PermissionViewJobRunLog), viewing the target run log
	// (PermissionViewTargetRunLog), viewing the receiver log
	// (PermissionViewReceiverLog), and viewing the audit log
	// (PermissionViewAuditLog) — those six are granted separately.
	PermissionView Permission = 1 << iota

	// PermissionDownload lets a session mint a download ticket and pull a
	// file's actual content (see handleMintDownloadTicket/handleDownloadFile
	// in webui.go). It implies PermissionView (see CanView) — there'd be no
	// way to discover a file to download without also being able to view
	// the file listing it comes from.
	PermissionDownload

	// PermissionAdmin lets a session retry a job's failed targets (see
	// handleRetryFailedTargets in webui.go). It implies PermissionView, PermissionDownload,
	// PermissionViewLoginLog, PermissionViewDownloadLog,
	// PermissionViewJobRunLog, PermissionViewTargetRunLog,
	// PermissionViewReceiverLog, and PermissionViewAuditLog (see
	// CanView/CanDownload/CanViewLoginLog/CanViewDownloadLog/
	// CanViewJobRunLog/CanViewTargetRunLog/CanViewReceiverLog/
	// CanViewAuditLog) — there'd be
	// reason to grant admin without also granting the rest of the dashboard.
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

	// PermissionViewJobRunLog lets a session see the dashboard's job run
	// log (see handleJobRunEvents in webui.go) — the history of completed
	// backup job runs across every job. Granted independently of
	// PermissionView, so a session holding only PermissionView can't see
	// it; implied by PermissionAdmin (see CanViewJobRunLog).
	PermissionViewJobRunLog

	// PermissionViewTargetRunLog lets a session see the dashboard's target
	// run log (see handleTargetRunEvents in webui.go) — the history of
	// completed backup job target runs across every job. Granted
	// independently of PermissionView, so a session holding only
	// PermissionView can't see it; implied by PermissionAdmin (see
	// CanViewTargetRunLog).
	PermissionViewTargetRunLog

	// PermissionViewReceiverLog lets a session see the dashboard's receiver
	// log (see handleReceiverEvents in webui.go) — the history of every
	// receiver API request (PUT or DELETE) this instance has served, win or
	// lose, across every receiver. Granted independently of PermissionView,
	// so a session holding only PermissionView can't see it; implied by
	// PermissionAdmin (see CanViewReceiverLog).
	PermissionViewReceiverLog

	// PermissionViewAuditLog lets a session see the dashboard's audit log
	// (see handleAuditEvents in webui.go) — every change a signed-in user
	// made (or tried to make) through the web UI, who made it, and when.
	// Granted independently of PermissionView, so a session holding only
	// PermissionView can't see it; implied by PermissionAdmin (see
	// CanViewAuditLog).
	PermissionViewAuditLog
)

// permissionNames maps each individual bit to its wire/config name, in
// canonical order. Shared by Names (bitmask -> names, for /api/me) and
// ParsePermissions (names -> bitmask, for the config file's
// webui.oidc.default-permissions: and webui.oidc.group-permissions:).
var permissionNames = []struct {
	bit  Permission
	name string
}{
	{PermissionView, "view"},
	{PermissionDownload, "download"},
	{PermissionAdmin, "admin"},
	{PermissionViewLoginLog, "login-log"},
	{PermissionViewDownloadLog, "download-log"},
	{PermissionViewJobRunLog, "job-run-log"},
	{PermissionViewTargetRunLog, "target-run-log"},
	{PermissionViewReceiverLog, "receiver-log"},
	{PermissionViewAuditLog, "audit-log"},
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

// CanAdmin reports whether p includes admin access (see PermissionAdmin).
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

// CanViewJobRunLog reports whether p includes the ability to see the
// dashboard's job run log — either granted directly, or implied by
// PermissionAdmin.
func (p Permission) CanViewJobRunLog() bool {
	return p&(PermissionViewJobRunLog|PermissionAdmin) != 0
}

// CanViewTargetRunLog reports whether p includes the ability to see the
// dashboard's target run log — either granted directly, or implied by
// PermissionAdmin.
func (p Permission) CanViewTargetRunLog() bool {
	return p&(PermissionViewTargetRunLog|PermissionAdmin) != 0
}

// CanViewReceiverLog reports whether p includes the ability to see the
// dashboard's receiver log — either granted directly, or implied by
// PermissionAdmin.
func (p Permission) CanViewReceiverLog() bool {
	return p&(PermissionViewReceiverLog|PermissionAdmin) != 0
}

// CanViewAuditLog reports whether p includes the ability to see the
// dashboard's audit log — either granted directly, or implied by
// PermissionAdmin.
func (p Permission) CanViewAuditLog() bool {
	return p&(PermissionViewAuditLog|PermissionAdmin) != 0
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

// ParsePermissions parses names (from the config file's
// webui.oidc.default-permissions: list or a webui.oidc.group-permissions:
// entry) into a Permission bitmask, rejecting any name not in permissionNames.
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
