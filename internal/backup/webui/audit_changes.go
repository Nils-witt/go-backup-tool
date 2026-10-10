package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/gpgkeys"
	"nilswitt.dev/go-backup-tool/internal/backup/jobs"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// redactedValue replaces a secret field's old/new value in an audit
// change: the change itself is still recorded, just not what it was.
const redactedValue = "(redacted)"

// auditResource is how recordChange reads one API collection's items so it
// can record what a change did to one (see diffSnapshots).
type auditResource struct {
	// snapshot returns the item target names as it's currently stored, in
	// the shape the dashboard edits it in, and false if there's none.
	snapshot func(ctx context.Context, target string) (any, bool, error)

	// secret reports whether field (a diffSnapshots path) holds a value the
	// audit log must not show; nil means none does.
	secret func(field string) bool
}

// auditResources returns the auditResource for every API collection
// recordChange can record field changes for, keyed by describeChange's
// resource name. Everything is read straight from db (and the GPG
// keyring), the same stored definitions the managers apply. A collection
// missing here (e.g. a job retry, which changes nothing stored) is
// recorded without field changes.
func auditResources(db *store.Store, keyring *gpgkeys.Keyring) map[string]auditResource {
	res := map[string]auditResource{
		"job-configs": {snapshot: getStored(db.GetJobConfig, jobs.FileJobFrom)},
		"server-configs": {snapshot: func(ctx context.Context, name string) (any, bool, error) {
			return findStored(ctx, db.ListServerConfigs, func(sc store.ServerConfig) (config.FileServer, bool) {
				return config.FileServer{Name: sc.Name, Type: sc.Type, Endpoint: sc.Endpoint, ServerUUID: sc.ServerUUID, Path: sc.Path, Retention: sc.Retention}, sc.Name == name
			})
		}},
		"command-configs": {snapshot: func(ctx context.Context, id string) (any, bool, error) {
			return findStored(ctx, db.ListCommandConfigs, func(cc store.CommandConfig) (config.FileCommand, bool) {
				return jobs.FileCommandFrom(cc), cc.ID == id
			})
		}},
		"receiver-configs": {snapshot: getStored(db.GetReceiverConfig, func(rc store.ReceiverConfig) config.FileReceiver {
			return config.FileReceiver{
				ID: rc.ID, AllowedServers: rc.AllowedServers, PublicKey: rc.PublicKey, Path: rc.Path, Retention: rc.Retention,
				StaleAfter: rc.StaleAfter, StaleNotifications: rc.StaleNotifications, DownloadNotifications: rc.DownloadNotifications,
			}
		})},
		"trusted-servers": {snapshot: func(ctx context.Context, id string) (any, bool, error) {
			return findStored(ctx, db.ListTrustedServers, func(ts store.TrustedServerConfig) (trust.Input, bool) {
				return trust.Input{ID: ts.ID, Name: ts.Name, PublicKey: ts.PublicKey}, ts.ID == id
			})
		}},
		"notification-configs": {
			snapshot: getStored(db.GetNotificationConfig, func(nc store.NotificationConfig) notify.FileNotification {
				return notify.FileNotification{ID: nc.ID, Webhook: nc.Webhook, Email: nc.Email}
			}),
			// A webhook's URL often embeds its credential (e.g. a chat
			// service's incoming-webhook token), and its headers' values are
			// write-only even to an admin (see webhookConfigJSON).
			secret: func(field string) bool {
				return field == "webhook.url" || strings.HasPrefix(field, "webhook.headers.")
			},
		},
		"report-config": {snapshot: getStored(func(ctx context.Context, _ string) (store.ReportSettings, bool, error) {
			return db.GetReportSettings(ctx)
		}, func(rs store.ReportSettings) report.FileReport {
			return report.FileReport{Enabled: rs.Enabled, Schedule: rs.Schedule, Notifications: rs.Notifications}
		})},
		// A token is revoked by id but created by name (its id is only
		// generated then), so either finds it: the newest token by that
		// name, for a create.
		"tokens": {snapshot: func(ctx context.Context, idOrName string) (any, bool, error) {
			return findStored(ctx, db.ListAPITokens, func(t store.APIToken) (apiTokenJSON, bool) {
				return apiTokenToJSON(t), t.ID == idOrName || t.Name == idOrName
			})
		}},
	}

	if keyring != nil {
		// The whole keyring is one item (an import has no target): each
		// key's fingerprint mapped to its user ids, so an import's change
		// lists the keys it added.
		res["gpg-keys"] = auditResource{snapshot: func(ctx context.Context, _ string) (any, bool, error) {
			keys, err := keyring.List(ctx)
			if err != nil {
				return nil, false, err
			}

			out := make(map[string][]string, len(keys))
			for _, k := range keys {
				out[k.Fingerprint] = k.UserIDs
			}

			return out, true, nil
		}}
	}

	return res
}

// getStored adapts a store Get* lookup into an auditResource snapshot,
// converting the stored row with conv.
func getStored[R, T any](get func(context.Context, string) (R, bool, error), conv func(R) T) func(context.Context, string) (any, bool, error) {
	return func(ctx context.Context, key string) (any, bool, error) {
		row, ok, err := get(ctx, key)
		if !ok || err != nil {
			return nil, false, err
		}

		return conv(row), true, nil
	}
}

// findStored returns the first of list's rows that match accepts, converted
// by match, and false if none does. list returns newest first where order
// matters (see tokens in auditResources).
func findStored[R, T any](ctx context.Context, list func(context.Context) ([]R, error), match func(R) (T, bool)) (any, bool, error) {
	rows, err := list(ctx)
	if err != nil {
		return nil, false, err
	}

	for _, r := range rows {
		if v, ok := match(r); ok {
			return v, true, nil
		}
	}

	return nil, false, nil
}

// diffSnapshots returns every field that differs between before and after
// — two snapshots of one item, either nil when the item didn't exist then
// — in field order, with secret fields' values redacted. Both are compared
// as their JSON encoding, flattened to one path per field (see
// flattenJSON); a field missing on one side counts as its zero value, so a
// create lists only the fields it set, and a delete only those that were
// set.
func diffSnapshots(before, after any, secret func(string) bool) ([]store.AuditChange, error) {
	old, err := flattenSnapshot(before)
	if err != nil {
		return nil, err
	}

	cur, err := flattenSnapshot(after)
	if err != nil {
		return nil, err
	}

	var changes []store.AuditChange

	for _, f := range unionKeys(old, cur) {
		o, n := nilIfZero(old[f]), nilIfZero(cur[f])
		if reflect.DeepEqual(o, n) {
			continue
		}

		if secret != nil && secret(f) {
			o, n = redact(o), redact(n)
		}

		changes = append(changes, store.AuditChange{Field: f, Old: o, New: n})
	}

	return changes, nil
}

// unionKeys returns every key in a or b, sorted.
func unionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}

	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	return keys
}

// flattenSnapshot JSON-encodes v and flattens it (see flattenJSON); nil
// flattens to no fields.
func flattenSnapshot(v any) (map[string]any, error) {
	out := map[string]any{}
	if v == nil {
		return out, nil
	}

	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding audit snapshot: %w", err)
	}

	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil, fmt.Errorf("decoding audit snapshot: %w", err)
	}

	flattenJSON("", decoded, out)

	return out, nil
}

// flattenJSON adds v's leaves to out, keyed by path: an object's fields as
// "parent.field", an array of objects or arrays by index as "parent[0]". An
// array of plain values stays one field, so e.g. a reordered recipient
// list reads as one change rather than one per position.
func flattenJSON(path string, v any, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[path] = t
			return
		}

		for k, child := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}

			flattenJSON(p, child, out)
		}
	case []any:
		if !slices.ContainsFunc(t, isComposite) {
			out[path] = t
			return
		}

		for i, child := range t {
			flattenJSON(fmt.Sprintf("%s[%d]", path, i), child, out)
		}
	default:
		out[path] = v
	}
}

func isComposite(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

// nilIfZero returns nil for v, a decoded JSON value, if it's null, "",
// false, 0, or an empty array or object, and v otherwise.
func nilIfZero(v any) any {
	zero := false

	switch t := v.(type) {
	case string:
		zero = t == ""
	case bool:
		zero = !t
	case float64:
		zero = t == 0
	case []any:
		zero = len(t) == 0
	case map[string]any:
		zero = len(t) == 0
	}

	if zero {
		return nil
	}

	return v
}

// redact replaces a set secret value with redactedValue, leaving an unset
// one nil so a create/delete still reads as one.
func redact(v any) any {
	if v == nil {
		return nil
	}

	return redactedValue
}
