package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// FileReceiver is one top-level receivers: entry, defining a path this
// instance accepts incoming objects into over its receiver API (see
// receiver.HandleReceiveObject), for pairing with
// another go-backup-tool instance's type: remote target. Only reachable
// when listen: is set, since the receiver API is served by the same HTTP
// server as the web UI dashboard.
type FileReceiver struct {
	ID string `yaml:"id" json:"id"`

	// Type is the receiver's kind: "backup" (the default when unset)
	// accepts whole backups from backup jobs; "directory" accepts single
	// files from on-change jobs and keeps their versions (see
	// ReceiverKindDirectory). It can't change once the receiver exists.
	Type string `yaml:"type,omitempty" json:"type"`

	// KeepVersions (directory receivers only) keeps at least this many of
	// the newest versions of every path. Together with Retention, a
	// version is deleted only once no rule set keeps it any more (see
	// objectstore.SweepFileVersions). 0 sets no version rule.
	KeepVersions int `yaml:"keep-versions,omitempty" json:"keep_versions"`

	// AllowedServers names the trusted servers (see internal/backup/trust)
	// allowed to write to this receiver, by server UUID. Every request must
	// present a JSON Web Token issued by one of them and signed with that
	// server's private key (see authorizeReceiver in internal/backup/receiver).
	// Set in the web UI or a settings file (see the settingsfile package).
	AllowedServers []string `yaml:"allowed-servers" json:"allowed_servers"`

	Path       string `yaml:"path" json:"path"`                         // root directory incoming objects for this id are written under
	Retention  string `yaml:"retention,omitempty" json:"retention"`     // optional, same syntax as a local server's retention: e.g. "30d"
	StaleAfter string `yaml:"stale-after,omitempty" json:"stale_after"` // optional, same duration syntax as retention: e.g. "6h" or "1d"; requires stale-notifications: and enables the stale-receiver monitor (see MonitorStaleReceivers)

	// StaleNotifications names notifications (managed in the web UI, see the
	// settings package) to fire once this receiver's most recent file turns
	// older than stale-after: (a receiver that has never received anything
	// never fires) — see MonitorStaleReceivers. Required together with
	// stale-after: (both, or neither).
	StaleNotifications []string `yaml:"stale-notifications,omitempty" json:"stale_notifications"`

	// DownloadNotifications names top-level notifications: entries to fire
	// every time a file is successfully downloaded from this receiver (see
	// handleDownloadFile/receiver.NotifyDownload); independent of
	// stale-after:/stale-notifications:. Optional.
	DownloadNotifications []string `yaml:"download-notifications,omitempty" json:"download_notifications"`

	// MaxObjectSize is the largest single object (or, on a directory
	// receiver, file) a sender may upload, as a size such as "10GiB" (see
	// ParseByteSize). Unset means DefaultReceiverMaxObjectSize.
	MaxObjectSize string `yaml:"max-object-size,omitempty" json:"max_object_size"`

	// Quota caps how much this receiver's path may hold in total, as a size
	// such as "1TB"; an upload that would take it over is rejected. Unset
	// means no quota.
	Quota string `yaml:"quota,omitempty" json:"quota"`

	// Immutable (backup receivers only) makes stored objects write-once: a
	// sender can't overwrite an object that already exists, so a
	// compromised sender can't destroy the backups it sent earlier. An
	// upload identical to the stored object succeeds without rewriting it,
	// so a retry after a lost response isn't stuck. Retention still
	// deletes expired objects.
	Immutable bool `yaml:"immutable,omitempty" json:"immutable"`
}

// ResolvedReceiver is one fileReceiver after validation, ready to be used by
// the receiver API's handlers.
type ResolvedReceiver struct {
	ID string

	// Kind is the receiver's type:. ResolveReceiver always sets it; an
	// empty Kind is treated as ReceiverKindBackup.
	Kind ReceiverKind

	// KeepVersions is a directory receiver's keep-versions:; 0 for a
	// backup receiver.
	KeepVersions int

	// AllowedServers are the trusted server ids allowed to write to this
	// receiver, looked up in TrustedServers on every request, so a key
	// rotated in the web UI applies immediately.
	AllowedServers []string
	TrustedServers *trust.Registry

	Path       string
	Retention  time.Duration
	StaleAfter time.Duration // 0 disables the stale-receiver monitor for this receiver

	// StaleNotifications/DownloadNotifications are this receiver's
	// notification ids, looked up in Notifications each time they fire, so
	// edits made in the web UI apply. An empty StaleNotifications means the
	// stale-receiver monitor is unset for this receiver (paired with
	// StaleAfter == 0); DownloadNotifications may be empty independent of
	// the other fields.
	StaleNotifications    []string
	DownloadNotifications []string

	// MaxObjectSize is the largest upload accepted, in bytes.
	// ResolveReceiver always sets it (DefaultReceiverMaxObjectSize when
	// max-object-size: is unset); 0 is treated as that default too. Quota
	// caps the bytes stored under Path; 0 means no quota.
	MaxObjectSize int64
	Quota         int64

	// Immutable is the receiver's immutable: setting (see FileReceiver).
	Immutable bool

	// Notifications is the live notification registry the ids above are
	// resolved against when firing (see notify.Registry.Resolve), copied
	// onto every receiver the same way ServerName is, so the stale/download
	// notifiers need no separate parameter. Nil resolves nothing.
	Notifications *notify.Registry

	// ServerName is the config file's top-level server-name: (see
	// fileConfig.ServerName), copied onto every receiver so
	// renderStaleWebhookPayload/renderDownloadWebhookPayload can substitute
	// it into a notification's {server_name} placeholder without a separate
	// parameter. "" when server-name: is unset.
	ServerName string
}

// ResolveReceiver validates one receiver definition — from the state db's
// receivers table, managed in the web UI or a settings file — requiring a
// non-empty id, at least one allowed server, and a non-empty path, and
// checking every notification id exists in notifications and every allowed
// server in trusted. A nil notifications or trusted skips that check. notifications, trusted, and serverName are copied onto
// the result (see ResolvedReceiver). Errors don't name the receiver;
// callers add that context.
func ResolveReceiver(fr FileReceiver, notifications *notify.Registry, trusted *trust.Registry, serverName string) (ResolvedReceiver, error) {
	id := strings.TrimSpace(fr.ID)
	if id == "" {
		return ResolvedReceiver{}, errors.New("id is required")
	}

	allowed, err := resolveAllowedServers(fr.AllowedServers, trusted)
	if err != nil {
		return ResolvedReceiver{}, err
	}

	if len(allowed) == 0 {
		return ResolvedReceiver{}, errors.New("an allowed server is required")
	}

	if strings.TrimSpace(fr.Path) == "" {
		return ResolvedReceiver{}, errors.New("path is required")
	}

	retention, err := parseRetention(fr.Retention)
	if err != nil {
		return ResolvedReceiver{}, err
	}

	kind, err := resolveReceiverKind(fr)
	if err != nil {
		return ResolvedReceiver{}, err
	}

	staleAfter, err := parseStaleAfter(fr.StaleAfter)
	if err != nil {
		return ResolvedReceiver{}, err
	}

	if err := CheckNotificationRefs(fr.StaleNotifications, notifications); err != nil {
		return ResolvedReceiver{}, fmt.Errorf("stale-notifications: %w", err)
	}

	if (staleAfter > 0) != (len(fr.StaleNotifications) > 0) {
		return ResolvedReceiver{}, errors.New("stale-after and stale-notifications must be set together")
	}

	if err := CheckNotificationRefs(fr.DownloadNotifications, notifications); err != nil {
		return ResolvedReceiver{}, fmt.Errorf("download-notifications: %w", err)
	}

	maxObjectSize, err := parseOptionalByteSize("max-object-size", fr.MaxObjectSize, DefaultReceiverMaxObjectSize)
	if err != nil {
		return ResolvedReceiver{}, err
	}

	quota, err := parseOptionalByteSize("quota", fr.Quota, 0)
	if err != nil {
		return ResolvedReceiver{}, err
	}

	return ResolvedReceiver{
		ID:                    id,
		Kind:                  kind,
		KeepVersions:          fr.KeepVersions,
		AllowedServers:        allowed,
		TrustedServers:        trusted,
		Path:                  fr.Path,
		Retention:             retention,
		StaleAfter:            staleAfter,
		StaleNotifications:    slices.Clone(fr.StaleNotifications),
		DownloadNotifications: slices.Clone(fr.DownloadNotifications),
		MaxObjectSize:         maxObjectSize,
		Quota:                 quota,
		Immutable:             fr.Immutable,
		Notifications:         notifications,
		ServerName:            serverName,
	}, nil
}

// ReceiverKind distinguishes a receiver's type:.
type ReceiverKind string

// The ReceiverKind values a receiver's type: can resolve to.
const (
	// ReceiverKindBackup (type: backup, or unset) stores whole backups
	// uploaded by backup jobs' remote targets.
	ReceiverKindBackup ReceiverKind = "backup"

	// ReceiverKindDirectory (type: directory) stores single files uploaded
	// by on-change jobs under the path they were sent with, keeping a new
	// version whenever a path's content changes.
	ReceiverKindDirectory ReceiverKind = "directory"
)

// ParseReceiverKind validates a FileReceiver's Type field; "" is
// ReceiverKindBackup.
func ParseReceiverKind(t string) (ReceiverKind, error) {
	switch strings.TrimSpace(t) {
	case "", string(ReceiverKindBackup):
		return ReceiverKindBackup, nil
	case string(ReceiverKindDirectory):
		return ReceiverKindDirectory, nil
	default:
		return "", fmt.Errorf("unknown type %q (want %q or %q)", t, ReceiverKindBackup, ReceiverKindDirectory)
	}
}

// resolveReceiverKind validates fr's type:, the keep-versions: that only a
// directory receiver may set, and the immutable: only a backup receiver
// may set.
func resolveReceiverKind(fr FileReceiver) (ReceiverKind, error) {
	kind, err := ParseReceiverKind(fr.Type)
	if err != nil {
		return "", err
	}

	switch {
	case fr.KeepVersions < 0:
		return "", fmt.Errorf("keep-versions must not be negative, got %d", fr.KeepVersions)
	case fr.KeepVersions > 0 && kind != ReceiverKindDirectory:
		return "", errors.New("keep-versions is only valid for a directory receiver (type: directory)")
	case fr.Immutable && kind == ReceiverKindDirectory:
		return "", errors.New("immutable is only valid for a backup receiver: a directory receiver never overwrites, it adds versions")
	}

	return kind, nil
}

// ValidateReceiverPath reports whether path — a receiver path set from the
// web UI — lies strictly inside baseDir (webui.receivers-base-dir:). Both
// must be absolute. Besides the lexical check, symlinks are resolved on
// baseDir and on path's deepest existing ancestor and the check repeated, so
// a symlink inside baseDir can't point a receiver somewhere else. An empty
// baseDir rejects every path: without it the web UI can't set paths at all.
func ValidateReceiverPath(baseDir, path string) error {
	if baseDir == "" {
		return errors.New("webui.receivers-base-dir is not configured, so receiver paths can't be set from the web UI")
	}

	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q must be absolute", path)
	}

	base := filepath.Clean(baseDir)
	clean := filepath.Clean(path)

	if !isStrictlyInside(base, clean) {
		return fmt.Errorf("path %q must be inside %s", path, base)
	}

	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("resolving webui.receivers-base-dir: %w", err)
	}

	realPath, err := evalExistingPrefix(clean)
	if err != nil {
		return fmt.Errorf("resolving path %q: %w", path, err)
	}

	if !isStrictlyInside(realBase, realPath) {
		return fmt.Errorf("path %q resolves to %s, outside %s", path, realPath, base)
	}

	return nil
}

// isStrictlyInside reports whether the clean absolute path p is a
// descendant of (not equal to) the clean absolute directory base.
func isStrictlyInside(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}

	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// evalExistingPrefix resolves symlinks in the longest existing prefix of the
// clean absolute path p, re-appending the not-yet-existing remainder — a new
// receiver's directory usually doesn't exist until its first write.
func evalExistingPrefix(p string) (string, error) {
	var rest []string

	cur := p

	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}

		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}

// CheckNotificationRefs reports the first of ids (a job's
// failure-notifications, a receiver's stale/download notifications, or the
// report's notifications) with no notification in notifications. A nil
// notifications checks nothing.
func CheckNotificationRefs(ids []string, notifications *notify.Registry) error {
	if notifications == nil {
		return nil
	}

	for i, id := range ids {
		if _, ok := notifications.Get(id); !ok {
			return fmt.Errorf("[%d]: unknown notification id %q", i, id)
		}
	}

	return nil
}

// resolveAllowedServers normalizes ids (see trust.NormalizeID), dropping
// duplicates, and checks each names a server in trusted (skipped when
// trusted is nil).
func resolveAllowedServers(ids []string, trusted *trust.Registry) ([]string, error) {
	var out []string

	for i, raw := range ids {
		id := trust.NormalizeID(raw)
		if id == "" {
			return nil, fmt.Errorf("allowed-servers[%d]: empty server id", i)
		}

		if slices.Contains(out, id) {
			continue
		}

		if trusted != nil {
			if _, ok := trusted.Get(id); !ok {
				return nil, fmt.Errorf("allowed-servers[%d]: unknown trusted server %q", i, id)
			}
		}

		out = append(out, id)
	}

	return out, nil
}

// parseStaleAfter parses a receiver's stale-after: string into a
// time.Duration, using the same "d for days" syntax as retention:
// (parseDayDuration). An empty string means the stale-receiver monitor is
// disabled for this receiver (the zero value); anything else must be
// positive, since "stale after zero (or a negative) time" is always true
// and so isn't a meaningful setting.
func parseStaleAfter(s string) (time.Duration, error) {
	return parseOptionalDayDuration("stale-after", s, false)
}
