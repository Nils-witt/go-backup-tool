package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// FileReceiver is one top-level receivers: entry, defining a path this
// instance accepts incoming objects into over its receiver API (see
// webui.go's handleReceiveObject/handleDeleteObject), for pairing with
// another go-backup-tool instance's type: remote target. Only reachable
// when listen: is set, since the receiver API is served by the same HTTP
// server as the web UI dashboard.
type FileReceiver struct {
	ID string `yaml:"id" json:"id"`

	// PublicKey is the PEM-encoded RSA public key of the sending instance
	// allowed to write to this receiver — the contents of that instance's
	// own generated data/keys/server.pub (see ensureServerKeyPair). Every
	// request must present a JSON Web Token signed with the matching
	// private key (see signRemoteAuthToken/verifyRemoteAuthToken and
	// authorizeReceiver in webui.go); unlike the token: field this replaces,
	// nothing here is itself a secret; it just names who's allowed to send.
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
	ID         string
	PublicKey  *rsa.PublicKey
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
// public-key:, and a non-empty path. notifications, if non-nil, is checked
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

		recv, err := ResolveReceiver(fr, notifications, serverName)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		receivers[id] = recv
	}

	return receivers, nil
}

// ResolveReceiver validates one receiver definition — from the config
// file's receivers: or from the state db's receivers table, managed in the
// web UI — requiring a non-empty id, a valid RSA public-key, and a
// non-empty path, and checking every notification id exists in
// notifications (skipped when notifications is nil, as when parsing the
// config file). notifications and serverName are copied onto the result
// (see ResolvedReceiver.Notifications/ServerName). Errors don't name the
// receiver; callers add that context.
func ResolveReceiver(fr FileReceiver, notifications *notify.Registry, serverName string) (ResolvedReceiver, error) {
	id := strings.TrimSpace(fr.ID)
	if id == "" {
		return ResolvedReceiver{}, errors.New("id is required")
	}

	publicKey, err := parseReceiverPublicKey(fr.PublicKey)
	if err != nil {
		return ResolvedReceiver{}, err
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

// parseReceiverPublicKey parses raw (a receiver's public-key: value) as a
// PEM-encoded PKIX public key — the same format ensureServerKeyPair writes
// to server.pub — requiring it to be an RSA key, since that's the only
// algorithm signRemoteAuthToken/verifyRemoteAuthToken sign and verify with.
func parseReceiverPublicKey(raw string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("public-key is required and must be a PEM-encoded PUBLIC KEY block")
	}

	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing public-key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public-key must be an RSA key, got %T", key)
	}

	return rsaKey, nil
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
