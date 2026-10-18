// Package settingsfile exports and imports the settings managed in the web
// UI (trusted servers, notifications, receivers and the report) as one YAML
// file, for keeping them in version control or copying them to another
// instance. Its notifications:, receivers: and report: sections have the
// same shape as the config file's former sections of those names, so a
// leftover section can be moved into a settings file as is.
//
// Every change goes through the same managers as the web UI, so an import
// is validated exactly like the same edit made there.
package settingsfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// File is a settings file's YAML document. Sections are listed in the order
// Import applies them: each may refer to ids from the ones before it.
type File struct {
	TrustedServers []TrustedServer           `yaml:"trusted-servers,omitempty"`
	Notifications  []notify.FileNotification `yaml:"notifications,omitempty"`
	Receivers      []config.FileReceiver     `yaml:"receivers,omitempty"`
	Report         *report.FileReport        `yaml:"report,omitempty"`
}

// TrustedServer is a trusted-servers: entry: another instance, by its
// server UUID, and the public key its requests are signed with (PEM).
type TrustedServer struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	PublicKey string `yaml:"public-key"`
}

// Managers are what Export reads from and Import writes through.
type Managers struct {
	Trust     *trust.Manager
	Settings  *settings.Manager
	Receivers *receiver.Manager
}

// Result counts what Import created and updated, per section.
type Result struct {
	Created, Updated map[string]int
}

// Export reads every stored setting into a File. The report is omitted if
// it was never saved.
func Export(ctx context.Context, m Managers) (File, error) {
	var f File

	servers, err := m.Trust.List(ctx)
	if err != nil {
		return File{}, fmt.Errorf("listing trusted servers: %w", err)
	}

	for _, s := range servers {
		f.TrustedServers = append(f.TrustedServers, TrustedServer{ID: s.ID, Name: s.Name, PublicKey: s.PublicKey})
	}

	notifications, err := m.Settings.ListNotifications(ctx)
	if err != nil {
		return File{}, fmt.Errorf("listing notifications: %w", err)
	}

	for _, n := range notifications {
		f.Notifications = append(f.Notifications, notify.FileNotification{ID: n.ID, Webhook: n.Webhook, Email: n.Email})
	}

	receivers, err := m.Receivers.List(ctx)
	if err != nil {
		return File{}, fmt.Errorf("listing receivers: %w", err)
	}

	for _, r := range receivers {
		f.Receivers = append(f.Receivers, r.FileReceiver())
	}

	rs, err := m.Settings.GetReport(ctx)
	if err != nil {
		return File{}, fmt.Errorf("reading report settings: %w", err)
	}

	if !rs.UpdatedAt.IsZero() {
		f.Report = &report.FileReport{Enabled: rs.Enabled, Schedule: rs.Schedule, Notifications: rs.Notifications}
	}

	return f, nil
}

// ExportFile writes Export's result to path as YAML, readable by the owner
// only: webhook header values (often credentials) are exported in clear.
func ExportFile(ctx context.Context, m Managers, path string) error {
	f, err := Export(ctx, m)
	if err != nil {
		return err
	}

	var buf bytes.Buffer

	fmt.Fprintf(&buf, "# go-backup-tool settings, exported %s.\n", time.Now().UTC().Format(time.RFC3339))
	buf.WriteString("# Contains webhook header values in clear text: keep this file private.\n")

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	if err := enc.Encode(f); err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing settings file: %w", err)
	}

	return nil
}

// Decode parses a settings file, rejecting unknown keys like the config
// file does.
func Decode(r io.Reader) (File, error) {
	var f File

	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("parsing settings file: %w", err)
	}

	return f, nil
}

// Import creates every entry of f that isn't stored yet and updates every
// one that is, by id, recording user as the author. It never deletes. It
// stops at the first entry that fails validation; entries applied before it
// stay applied, and running the import again after fixing the file is safe.
func Import(ctx context.Context, m Managers, user string, f File) (Result, error) {
	res := Result{Created: map[string]int{}, Updated: map[string]int{}}

	for _, s := range f.TrustedServers {
		in := trust.Input{ID: s.ID, Name: s.Name, PublicKey: s.PublicKey}
		if err := upsert(&res, "trusted-servers", store.ErrTrustedServerExists,
			func() error { return m.Trust.Create(ctx, user, in) },
			func() error { return m.Trust.Update(ctx, user, in) }); err != nil {
			return res, fmt.Errorf("trusted server %q: %w", s.ID, err)
		}
	}

	for _, n := range f.Notifications {
		in := notificationInput(n)
		if err := upsert(&res, "notifications", store.ErrNotificationExists,
			func() error { return m.Settings.CreateNotification(ctx, user, in) },
			func() error { return m.Settings.UpdateNotification(ctx, user, in) }); err != nil {
			return res, fmt.Errorf("notification %q: %w", n.ID, err)
		}
	}

	for _, r := range f.Receivers {
		if err := upsert(&res, "receivers", store.ErrReceiverExists,
			func() error { return m.Receivers.Create(ctx, user, r) },
			func() error { return m.Receivers.Update(ctx, user, r) }); err != nil {
			return res, fmt.Errorf("receiver %q: %w", r.ID, err)
		}
	}

	if f.Report != nil {
		if err := m.Settings.UpdateReport(ctx, user, *f.Report); err != nil {
			return res, fmt.Errorf("report: %w", err)
		}

		res.Updated["report"]++
	}

	return res, nil
}

// ImportFile decodes the settings file at path and imports it (see Import).
func ImportFile(ctx context.Context, m Managers, user, path string) (Result, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator's own -import-settings argument
	if err != nil {
		return Result{}, fmt.Errorf("reading settings file: %w", err)
	}

	f, err := Decode(bytes.NewReader(data))
	if err != nil {
		return Result{}, err
	}

	return Import(ctx, m, user, f)
}

// upsert runs create, and update instead when create reports the id as
// taken (exists), counting the outcome under section.
func upsert(res *Result, section string, exists error, create, update func() error) error {
	err := create()
	if err == nil {
		res.Created[section]++
		return nil
	}

	if !errors.Is(err, exists) {
		return err
	}

	if err := update(); err != nil {
		return err
	}

	res.Updated[section]++

	return nil
}

// notificationInput turns a file notification into the web UI's input
// shape, with every header value given explicitly.
func notificationInput(n notify.FileNotification) settings.NotificationInput {
	in := settings.NotificationInput{ID: n.ID, Email: n.Email}
	if n.Webhook == nil {
		return in
	}

	headers := make(map[string]*string, len(n.Webhook.Headers))
	for name, value := range n.Webhook.Headers {
		headers[name] = &value
	}

	in.Webhook = &settings.WebhookInput{URL: n.Webhook.URL, Method: n.Webhook.Method, Headers: headers, Body: n.Webhook.Body}

	return in
}
