// Package settings manages the notifications and report settings an admin
// edits in the web UI: stored in the state db, resolved into the live
// notify.Registry and report.Live every trigger reads at fire time, and
// imported once from the config file's deprecated notifications:/report:
// entries.
package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var (
	// ErrInvalid wraps every validation failure a Manager mutation returns,
	// so the web UI can answer 400 with the message.
	ErrInvalid = errors.New("invalid settings")
	// ErrStoreUnavailable is returned by every Manager mutation when the
	// state db couldn't be opened at startup.
	ErrStoreUnavailable = errors.New("state db unavailable: settings can't be changed this run")
	// ErrNotificationInUse is returned by DeleteNotification for a
	// notification something still references.
	ErrNotificationInUse = errors.New("notification is still in use")
)

// configFileUser is recorded as created_by/updated_by on settings imported
// from the config file.
const configFileUser = "config file"

// Manager owns the notifications' and report settings' lifecycle: stored in
// the state db, resolved into the live notification registry and report
// settings, so an edit in the web UI takes effect everywhere immediately.
// It's the only writer of all of them.
type Manager struct {
	db            *store.Store
	notifications *notify.Registry
	report        *report.Live
	smtp          notify.SMTPSettings
	gpg           notify.GPGSettings
	jobs          []*config.Config
	log           *slog.Logger

	// mu serializes mutations, so the db and the live state never disagree.
	mu sync.Mutex
	// invalid maps each stored notification id that failed to resolve at
	// Load to why; reportErr is why the stored report settings failed to,
	// "" if they didn't.
	invalid   map[string]string
	reportErr string
}

// NewManager builds a Manager over db (nil if the state db couldn't be
// opened), resolving notifications into notifications and the report into
// live. smtp/gpg are what email notifications send and encrypt with (they
// stay in the config file); jobs are checked for references before a
// notification is deleted. Call Load before anything reads the registry.
func NewManager(db *store.Store, notifications *notify.Registry, live *report.Live, smtp notify.SMTPSettings, gpg notify.GPGSettings, jobs []*config.Config, log *slog.Logger) *Manager {
	return &Manager{
		db: db, notifications: notifications, report: live, smtp: smtp, gpg: gpg, jobs: jobs,
		log: log, invalid: make(map[string]string),
	}
}

// SMTPConfigured reports whether the config file's smtp: is set, i.e.
// whether email notifications can be created.
func (m *Manager) SMTPConfigured() bool { return m.smtp.Host != "" }

// NotificationIDs returns every active notification id, sorted.
func (m *Manager) NotificationIDs() []string { return m.notifications.IDs() }

// Load imports yamlNotifications/yamlReport (the config file's deprecated
// notifications:/report:) into the state db — only what isn't stored yet,
// so edits made in the web UI since are kept — then resolves everything
// stored into the live registry and report settings. Something stored that
// no longer resolves is logged and left inactive (see ListNotifications/
// GetReport). With no state db, the config file's entries are resolved in
// memory instead. Finally, every job's failure-notifications ids are
// checked, logging any that don't exist.
func (m *Manager) Load(ctx context.Context, yamlNotifications []notify.FileNotification, yamlReport report.FileReport) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.db == nil {
		for _, fn := range yamlNotifications {
			m.activateNotificationLocked(fn)
		}

		m.activateReportLocked(yamlReport)
		m.checkJobRefsLocked()

		return
	}

	m.importLocked(ctx, yamlNotifications, yamlReport)

	stored, err := m.db.ListNotificationConfigs(ctx)
	if err != nil {
		m.log.Error("reading notifications from the state db", "err", err)
	}

	for _, nc := range stored {
		m.activateNotificationLocked(notify.FileNotification{ID: nc.ID, Webhook: nc.Webhook, Email: nc.Email})
	}

	rs, ok, err := m.db.GetReportSettings(ctx)
	if err != nil {
		m.log.Error("reading report settings from the state db", "err", err)
	} else if ok {
		m.activateReportLocked(report.FileReport{Enabled: rs.Enabled, Schedule: rs.Schedule, Notifications: rs.Notifications})
	}

	m.checkJobRefsLocked()
}

// importLocked carries the config file's deprecated notifications:/report:
// over into the state db. m.mu must be held.
func (m *Manager) importLocked(ctx context.Context, yamlNotifications []notify.FileNotification, yamlReport report.FileReport) {
	now := time.Now()

	if len(yamlNotifications) > 0 {
		rows := make([]store.NotificationConfig, len(yamlNotifications))
		for i, fn := range yamlNotifications {
			rows[i] = store.NotificationConfig{
				ID: strings.TrimSpace(fn.ID), Webhook: fn.Webhook, Email: fn.Email,
				CreatedAt: now, CreatedBy: configFileUser, UpdatedAt: now, UpdatedBy: configFileUser,
			}
		}

		imported, err := m.db.ImportNotificationConfigs(ctx, rows)
		if err != nil {
			m.log.Error("importing config file notifications into the state db", "err", err)
		}

		m.log.Warn("notifications: in the config file is deprecated: notifications are managed in the web UI now; remove the notifications: section from the config file",
			"imported", imported, "already_stored", len(yamlNotifications)-len(imported))
	}

	if yamlReport.Enabled || yamlReport.Schedule != "" || len(yamlReport.Notifications) > 0 {
		imported, err := m.db.ImportReportSettings(ctx, store.ReportSettings{
			Enabled: yamlReport.Enabled, Schedule: yamlReport.Schedule, Notifications: yamlReport.Notifications,
			UpdatedAt: now, UpdatedBy: configFileUser,
		})
		if err != nil {
			m.log.Error("importing config file report: into the state db", "err", err)
		}

		m.log.Warn("report: in the config file is deprecated: the report is managed in the web UI now; remove the report: section from the config file",
			"imported", imported)
	}
}

// activateNotificationLocked resolves fn into the registry, or records why
// it doesn't resolve. m.mu must be held.
func (m *Manager) activateNotificationLocked(fn notify.FileNotification) {
	n, err := notify.ResolveNotification(fn, m.smtp, m.gpg)
	if err != nil {
		m.log.Error("notification is invalid and stays inactive until fixed in the web UI", "id", fn.ID, "err", err)
		m.invalid[fn.ID] = err.Error()

		return
	}

	delete(m.invalid, n.ID)
	m.notifications.Put(n)
}

// activateReportLocked resolves fr into the live report settings, or
// records why it doesn't resolve (leaving the report disabled). m.mu must
// be held.
func (m *Manager) activateReportLocked(fr report.FileReport) {
	s, err := report.ResolveSettings(fr, m.notifications)
	if err != nil {
		m.log.Error("report settings are invalid; the report stays disabled until fixed in the web UI", "err", err)
		m.reportErr = err.Error()
		m.report.Set(report.Settings{})

		return
	}

	m.reportErr = ""
	m.report.Set(s)
}

// checkJobRefsLocked logs every job failure-notifications id with no
// active notification: the config file can't be checked for these while
// it's parsed, since notifications live in the state db.
func (m *Manager) checkJobRefsLocked() {
	for _, job := range m.jobs {
		if err := config.CheckNotificationRefs(job.FailureNotifications, m.notifications); err != nil {
			m.log.Error("job failure-notifications references a notification that doesn't exist; it will be skipped", "job", job.Name, "err", err)
		}
	}
}

// ManagedNotification is one stored notification as the web UI lists it:
// its definition as entered, Error when it no longer resolves (and so is
// inactive), and UsedBy, what references it (see usersOf).
type ManagedNotification struct {
	store.NotificationConfig

	Error  string
	UsedBy []string
}

// ListNotifications returns every stored notification, in id order.
func (m *Manager) ListNotifications(ctx context.Context) ([]ManagedNotification, error) {
	if m.db == nil {
		return nil, ErrStoreUnavailable
	}

	stored, err := m.db.ListNotificationConfigs(ctx)
	if err != nil {
		return nil, err
	}

	users, err := m.usersByNotification(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]ManagedNotification, len(stored))
	for i, nc := range stored {
		out[i] = ManagedNotification{NotificationConfig: nc, Error: m.invalid[nc.ID], UsedBy: users[nc.ID]}
	}

	return out, nil
}

// WebhookInput is a notification's webhook channel as the web UI submits
// it. Header values are write-only: a nil value keeps the header's stored
// value (an error if it has none), so the web UI never needs to read them
// back.
type WebhookInput struct {
	URL     string             `json:"url"`
	Method  string             `json:"method"`
	Headers map[string]*string `json:"headers"`
	Body    string             `json:"body"`
}

// NotificationInput is a notification as the web UI submits it.
type NotificationInput struct {
	ID      string            `json:"id"`
	Webhook *WebhookInput     `json:"webhook"`
	Email   *notify.FileEmail `json:"email"`
}

// CreateNotification validates and stores a new notification and activates
// it. Validation failures wrap ErrInvalid; a taken id returns
// store.ErrNotificationExists.
func (m *Manager) CreateNotification(ctx context.Context, user string, in NotificationInput) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	fn, n, err := m.validateNotification(in, nil)
	if err != nil {
		return err
	}

	now := time.Now()
	if err := m.db.CreateNotificationConfig(ctx, store.NotificationConfig{
		ID: fn.ID, Webhook: fn.Webhook, Email: fn.Email, CreatedAt: now, CreatedBy: user, UpdatedAt: now, UpdatedBy: user,
	}); err != nil {
		return err
	}

	delete(m.invalid, n.ID)
	m.notifications.Put(n)
	m.log.Info("notification created", "id", n.ID, "by", user)

	return nil
}

// UpdateNotification validates and stores notification in.ID's new
// definition and activates it; everything referencing it picks the change
// up on its next firing. Validation failures wrap ErrInvalid; an unknown id
// returns store.ErrNotificationNotFound.
func (m *Manager) UpdateNotification(ctx context.Context, user string, in NotificationInput) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok, err := m.db.GetNotificationConfig(ctx, strings.TrimSpace(in.ID))
	if err != nil {
		return err
	}

	if !ok {
		return store.ErrNotificationNotFound
	}

	var storedHeaders map[string]string
	if existing.Webhook != nil {
		storedHeaders = existing.Webhook.Headers
	}

	fn, n, err := m.validateNotification(in, storedHeaders)
	if err != nil {
		return err
	}

	if err := m.db.UpdateNotificationConfig(ctx, store.NotificationConfig{
		ID: fn.ID, Webhook: fn.Webhook, Email: fn.Email, UpdatedAt: time.Now(), UpdatedBy: user,
	}); err != nil {
		return err
	}

	delete(m.invalid, n.ID)
	m.notifications.Put(n)
	m.log.Info("notification updated", "id", n.ID, "by", user)

	return nil
}

// DeleteNotification removes notification id, refusing (ErrNotificationInUse,
// naming the users) while a job, receiver, or the report references it. An
// unknown id returns store.ErrNotificationNotFound.
func (m *Manager) DeleteNotification(ctx context.Context, user, id string) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	users, err := m.usersByNotification(ctx)
	if err != nil {
		return err
	}

	if u := users[id]; len(u) > 0 {
		return fmt.Errorf("%w by %s", ErrNotificationInUse, strings.Join(u, ", "))
	}

	if err := m.db.DeleteNotificationConfig(ctx, id); err != nil {
		return err
	}

	delete(m.invalid, id)
	m.notifications.Delete(id)
	m.log.Info("notification deleted", "id", id, "by", user)

	return nil
}

// usersByNotification maps each notification id to what references it:
// "job <name>" for a job's failure-notifications, "receiver <id>" for a
// stored receiver's stale/download notifications, and "report" for the
// stored report settings (even while disabled, so re-enabling it can't
// break).
func (m *Manager) usersByNotification(ctx context.Context) (map[string][]string, error) {
	users := make(map[string][]string)
	add := func(ids []string, user string) {
		for _, id := range ids {
			if !slices.Contains(users[id], user) {
				users[id] = append(users[id], user)
			}
		}
	}

	for _, job := range m.jobs {
		add(job.FailureNotifications, "job "+job.Name)
	}

	receivers, err := m.db.ListReceiverConfigs(ctx)
	if err != nil {
		return nil, err
	}

	for _, rc := range receivers {
		add(rc.StaleNotifications, "receiver "+rc.ID)
		add(rc.DownloadNotifications, "receiver "+rc.ID)
	}

	rs, ok, err := m.db.GetReportSettings(ctx)
	if err != nil {
		return nil, err
	}

	if ok {
		add(rs.Notifications, "report")
	}

	return users, nil
}

// validateNotification turns in into a stored definition — filling in
// write-only header values from storedHeaders — and resolves it, wrapping
// any failure in ErrInvalid.
func (m *Manager) validateNotification(in NotificationInput, storedHeaders map[string]string) (notify.FileNotification, notify.Notification, error) {
	fn := notify.FileNotification{ID: strings.TrimSpace(in.ID), Email: in.Email}

	if in.Webhook != nil {
		wh, err := webhookFromInput(*in.Webhook, storedHeaders)
		if err != nil {
			return notify.FileNotification{}, notify.Notification{}, fmt.Errorf("%w: webhook: %w", ErrInvalid, err)
		}

		fn.Webhook = wh
	}

	n, err := notify.ResolveNotification(fn, m.smtp, m.gpg)
	if err != nil {
		return notify.FileNotification{}, notify.Notification{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return fn, n, nil
}

// webhookFromInput validates in beyond notify.ResolveNotification's own
// checks — an http(s) URL, well-formed header names and values — and
// resolves its write-only header values against storedHeaders.
func webhookFromInput(in WebhookInput, storedHeaders map[string]string) (*notify.FileWebhook, error) {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("url must be an absolute http:// or https:// URL")
	}

	var headers map[string]string

	for name, value := range in.Headers {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t\r\n:") {
			return nil, fmt.Errorf("invalid header name %q", name)
		}

		var v string

		switch stored, ok := storedHeaders[name]; {
		case value != nil:
			v = *value
		case ok:
			v = stored
		default:
			return nil, fmt.Errorf("header %q: value is required", name)
		}

		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("header %q: value must be a single line", name)
		}

		if headers == nil {
			headers = make(map[string]string, len(in.Headers))
		}

		headers[name] = v
	}

	return &notify.FileWebhook{URL: u.String(), Method: in.Method, Headers: headers, Body: in.Body}, nil
}

// NextReport returns when the report is next due after now, reporting
// false while it's disabled.
func (m *Manager) NextReport(now time.Time) (time.Time, bool) {
	s := m.report.Get()
	if !s.Enabled {
		return time.Time{}, false
	}

	return s.Schedule.Next(now.UTC()), true
}

// ManagedReport is the stored report settings as the web UI shows them:
// as entered, with Error set when they no longer resolve (the report is
// then disabled).
type ManagedReport struct {
	store.ReportSettings

	Error string
}

// GetReport returns the stored report settings — the zero value (disabled,
// default schedule) if none were ever saved.
func (m *Manager) GetReport(ctx context.Context) (ManagedReport, error) {
	if m.db == nil {
		return ManagedReport{}, ErrStoreUnavailable
	}

	rs, _, err := m.db.GetReportSettings(ctx)
	if err != nil {
		return ManagedReport{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return ManagedReport{ReportSettings: rs, Error: m.reportErr}, nil
}

// UpdateReport validates and stores fr as the report settings and applies
// them; the report loop reschedules immediately. Validation failures wrap
// ErrInvalid.
func (m *Manager) UpdateReport(ctx context.Context, user string, fr report.FileReport) error {
	if m.db == nil {
		return ErrStoreUnavailable
	}

	fr.Schedule = strings.TrimSpace(fr.Schedule)

	m.mu.Lock()
	defer m.mu.Unlock()

	s, err := report.ResolveSettings(fr, m.notifications)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	if err := m.db.SaveReportSettings(ctx, store.ReportSettings{
		Enabled: fr.Enabled, Schedule: fr.Schedule, Notifications: fr.Notifications, UpdatedAt: time.Now(), UpdatedBy: user,
	}); err != nil {
		return err
	}

	m.reportErr = ""
	m.report.Set(s)
	m.log.Info("report settings updated", "enabled", fr.Enabled, "schedule", fr.Schedule, "by", user)

	return nil
}
