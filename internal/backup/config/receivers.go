package config

import (
	"crypto/rsa"
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
// webui.go's handleReceiveObject/handleDeleteObject), for pairing with
// another go-backup-tool instance's type: remote target. Only reachable
// when listen: is set, since the receiver API is served by the same HTTP
// server as the web UI dashboard.
type FileReceiver struct {
	ID string `yaml:"id" json:"id"`

	// AllowedServers names the trusted servers (see internal/backup/trust)
	// allowed to write to this receiver, by server UUID. Every request must
	// present a JSON Web Token issued by one of them and signed with that
	// server's private key (see authorizeReceiver in internal/backup/receiver).
	// Managed in the web UI only, so not read from the config file.
	AllowedServers []string `yaml:"-" json:"allowed_servers"`

	// PublicKey is the deprecated way of naming a receiver's sender: the
	// PEM-encoded RSA public key of the one sending instance allowed to
	// write to it, accepted from any issuer. Still honored alongside
	// AllowedServers so receivers set up before trusted servers existed
	// keep working; optional once AllowedServers is set.
	PublicKey  string `yaml:"public-key" json:"public_key"`
	Path       string `yaml:"path" json:"path"`               // root directory incoming objects for this id are written under
	Retention  string `yaml:"retention" json:"retention"`     // optional, same syntax as a local server's retention: e.g. "30d"
	StaleAfter string `yaml:"stale-after" json:"stale_after"` // optional, same duration syntax as retention: e.g. "6h" or "1d"; requires stale-notifications: and enables the stale-receiver monitor (see MonitorStaleReceivers)

	// StaleNotifications names top-level notifications: entries (see
	// notify.Build) to fire once this receiver's most recent file turns
	// older than stale-after: (a receiver that has never received anything
	// never fires) — see MonitorStaleReceivers. Required together with
	// stale-after: (both, or neither).
	StaleNotifications []string `yaml:"stale-notifications" json:"stale_notifications"`

	// DownloadNotifications names top-level notifications: entries to fire
	// every time a file is successfully downloaded from this receiver (see
	// handleDownloadFile/receiver.NotifyDownload); independent of
	// stale-after:/stale-notifications:. Optional.
	DownloadNotifications []string `yaml:"download-notifications" json:"download_notifications"`
}

// ResolvedReceiver is one fileReceiver after validation, ready to be used by
// the receiver API's handlers.
type ResolvedReceiver struct {
	ID string

	// AllowedServers are the trusted server ids allowed to write to this
	// receiver, looked up in TrustedServers on every request, so a key
	// rotated in the web UI applies immediately. PublicKey is the
	// deprecated sender key (see FileReceiver.PublicKey), nil when unset.
	AllowedServers []string
	TrustedServers *trust.Registry
	PublicKey      *rsa.PublicKey

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

// buildReceivers validates fileReceivers and builds an id -> resolvedReceiver
// map, requiring every entry to have a unique, non-empty id, a valid RSA
// public-key: (the config file can't name trusted servers), and a non-empty
// path. notifications, if non-nil, is checked
// for every stale-notifications:/download-notifications: id (see
// ResolveReceiver). serverName is
// the config file's top-level server-name:, copied onto every resolved
// receiver (see ResolvedReceiver.ServerName).
func buildReceivers(fileReceivers []FileReceiver, notifications *notify.Registry, serverName string) (map[string]ResolvedReceiver, error) {
	receivers := make(map[string]ResolvedReceiver, len(fileReceivers))

	for i, fr := range fileReceivers {
		id := strings.TrimSpace(fr.ID)
		if id == "" {
			return nil, fmt.Errorf("receivers[%d]: id is required", i)
		}

		if _, exists := receivers[id]; exists {
			return nil, fmt.Errorf("receivers[%d]: duplicate receiver id %q", i, id)
		}

		// The config file can't name trusted servers, so its receivers
		// still need the (deprecated) public-key:.
		if strings.TrimSpace(fr.PublicKey) == "" {
			return nil, fmt.Errorf("receiver %q: public-key is required", id)
		}

		recv, err := ResolveReceiver(fr, notifications, nil, serverName)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		receivers[id] = recv
	}

	return receivers, nil
}

// ResolveReceiver validates one receiver definition — from the config
// file's receivers: or from the state db's receivers table, managed in the
// web UI — requiring a non-empty id, at least one allowed server or a
// (deprecated) public-key, and a non-empty path, and checking every
// notification id exists in notifications and every allowed server in
// trusted. A nil notifications or trusted skips that check, as when parsing
// the config file. notifications, trusted, and serverName are copied onto
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

	var publicKey *rsa.PublicKey

	switch {
	case strings.TrimSpace(fr.PublicKey) != "":
		if publicKey, err = trust.ParsePublicKey(fr.PublicKey); err != nil {
			return ResolvedReceiver{}, err
		}
	case len(allowed) == 0:
		return ResolvedReceiver{}, errors.New("an allowed server (or the deprecated public-key) is required")
	}

	if strings.TrimSpace(fr.Path) == "" {
		return ResolvedReceiver{}, errors.New("path is required")
	}

	retention, err := parseRetention(fr.Retention)
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

	return ResolvedReceiver{
		ID:                    id,
		AllowedServers:        allowed,
		TrustedServers:        trusted,
		PublicKey:             publicKey,
		Path:                  fr.Path,
		Retention:             retention,
		StaleAfter:            staleAfter,
		StaleNotifications:    slices.Clone(fr.StaleNotifications),
		DownloadNotifications: slices.Clone(fr.DownloadNotifications),
		Notifications:         notifications,
		ServerName:            serverName,
	}, nil
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
