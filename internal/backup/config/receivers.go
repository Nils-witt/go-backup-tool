package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
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
	ID string `yaml:"id"`

	// PublicKey is the PEM-encoded RSA public key of the sending instance
	// allowed to write to this receiver — the contents of that instance's
	// own generated data/keys/server.pub (see ensureServerKeyPair). Every
	// request must present a JSON Web Token signed with the matching
	// private key (see signRemoteAuthToken/verifyRemoteAuthToken and
	// authorizeReceiver in webui.go); unlike the token: field this replaces,
	// nothing here is itself a secret; it just names who's allowed to send.
	PublicKey  string `yaml:"public-key"`
	Path       string `yaml:"path"`        // root directory incoming objects for this id are written under
	Retention  string `yaml:"retention"`   // optional, same syntax as a local server's retention: e.g. "30d"
	StaleAfter string `yaml:"stale-after"` // optional, same duration syntax as retention: e.g. "6h" or "1d"; requires stale-notifications: and enables the stale-receiver monitor (see MonitorStaleReceivers)

	// StaleNotifications names top-level notifications: entries (see
	// notify.Build) to fire once this receiver's most recent file turns
	// older than stale-after: (a receiver that has never received anything
	// never fires) — see MonitorStaleReceivers. Required together with
	// stale-after: (both, or neither).
	StaleNotifications []string `yaml:"stale-notifications"`

	// DownloadNotifications names top-level notifications: entries to fire
	// every time a file is successfully downloaded from this receiver (see
	// handleDownloadFile/receiver.NotifyDownload); independent of
	// stale-after:/stale-notifications:. Optional.
	DownloadNotifications []string `yaml:"download-notifications"`
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
	// StaleNotifications/DownloadNotifications ids resolved against the
	// config file's top-level notifications: (see notify.Build). An empty
	// StaleNotifications means the stale-receiver monitor is unset for this
	// receiver (paired with StaleAfter == 0); DownloadNotifications may be
	// empty independent of the other fields.
	StaleNotifications    []notify.Notification
	DownloadNotifications []notify.Notification

	// ServerName is the config file's top-level server-name: (see
	// fileConfig.ServerName), copied onto every receiver so
	// renderStaleWebhookPayload/renderDownloadWebhookPayload can substitute
	// it into a notification's {server_name} placeholder without a separate
	// parameter. "" when server-name: is unset.
	ServerName string
}

// buildReceivers validates fileReceivers and builds an id -> resolvedReceiver
// map, requiring every entry to have a unique, non-empty id, a valid RSA
// public-key:, and a non-empty path. notifications is the config file's
// already-resolved top-level notifications: map (see notify.Build), used to
// resolve stale-notifications:/download-notifications: ids. serverName is
// the config file's top-level server-name:, copied onto every resolved
// receiver (see ResolvedReceiver.ServerName).
func buildReceivers(fileReceivers []FileReceiver, notifications map[string]notify.Notification, serverName string) (map[string]ResolvedReceiver, error) {
	receivers := make(map[string]ResolvedReceiver, len(fileReceivers))

	for i, fr := range fileReceivers {
		id := strings.TrimSpace(fr.ID)
		if id == "" {
			return nil, fmt.Errorf("receivers[%d]: id is required", i)
		}

		if _, exists := receivers[id]; exists {
			return nil, fmt.Errorf("receivers[%d]: duplicate receiver id %q", i, id)
		}

		publicKey, err := parseReceiverPublicKey(fr.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		if strings.TrimSpace(fr.Path) == "" {
			return nil, fmt.Errorf("receiver %q: path is required", id)
		}

		retention, err := parseRetention(fr.Retention)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		staleAfter, err := parseStaleAfter(fr.StaleAfter)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		staleNotifications, err := resolveNotificationRefs(fr.StaleNotifications, notifications)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: stale-notifications: %w", id, err)
		}

		if (staleAfter > 0) != (len(staleNotifications) > 0) {
			return nil, fmt.Errorf("receiver %q: stale-after and stale-notifications must be set together", id)
		}

		downloadNotifications, err := resolveNotificationRefs(fr.DownloadNotifications, notifications)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: download-notifications: %w", id, err)
		}

		receivers[id] = ResolvedReceiver{
			ID:                    id,
			PublicKey:             publicKey,
			Path:                  fr.Path,
			Retention:             retention,
			StaleAfter:            staleAfter,
			StaleNotifications:    staleNotifications,
			DownloadNotifications: downloadNotifications,
			ServerName:            serverName,
		}
	}

	return receivers, nil
}

// resolveNotificationRefs resolves ids (a job's failure-notifications: or a
// receiver's stale-notifications:/download-notifications: list) against
// notifications (the config file's top-level notifications: map, see
// notify.Build), erroring on any id with no matching entry. A nil/empty ids
// returns nil.
func resolveNotificationRefs(ids []string, notifications map[string]notify.Notification) ([]notify.Notification, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	resolved := make([]notify.Notification, len(ids))

	for i, id := range ids {
		n, ok := notifications[id]
		if !ok {
			return nil, fmt.Errorf("[%d]: unknown notification id %q", i, id)
		}

		resolved[i] = n
	}

	return resolved, nil
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
