package webui

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// maxSettingsBody bounds a notification/report create or update request
// body; a webhook body template is the largest field.
const maxSettingsBody = 256 << 10

// webhookConfigJSON is a stored webhook as /api/notification-configs lists
// it. Header values are write-only, so every value is null: the dashboard
// sends null back to keep a header's stored value (see
// settings.WebhookInput).
type webhookConfigJSON struct {
	URL     string             `json:"url"`
	Method  string             `json:"method"`
	Headers map[string]*string `json:"headers"`
	Body    string             `json:"body"`
}

// notificationConfigJSON is one stored notification's wire shape for
// /api/notification-configs (see settings.ManagedNotification).
type notificationConfigJSON struct {
	ID        string             `json:"id"`
	Webhook   *webhookConfigJSON `json:"webhook"`
	Email     *notify.FileEmail  `json:"email"`
	CreatedAt time.Time          `json:"created_at"`
	CreatedBy string             `json:"created_by"`
	UpdatedAt time.Time          `json:"updated_at"`
	UpdatedBy string             `json:"updated_by"`
	Error     string             `json:"error,omitempty"`
	UsedBy    []string           `json:"used_by"`
}

// notificationConfigListJSON is GET /api/notification-configs' response.
type notificationConfigListJSON struct {
	// SMTPConfigured reports whether the config file's smtp: is set, which
	// email notifications need.
	SMTPConfigured bool                     `json:"smtp_configured"`
	Notifications  []notificationConfigJSON `json:"notifications"`
}

// handleListNotificationConfigs serves GET /api/notification-configs
// (admin only).
func handleListNotificationConfigs(m *settings.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.ListNotifications(r.Context())
		if err != nil {
			writeSettingsError(w, log, "listing notifications", "", err)
			return
		}

		out := notificationConfigListJSON{SMTPConfigured: m.SMTPConfigured(), Notifications: make([]notificationConfigJSON, len(list))}

		for i, mn := range list {
			out.Notifications[i] = notificationConfigJSON{
				ID: mn.ID, Webhook: maskWebhook(mn.Webhook), Email: mn.Email,
				CreatedAt: mn.CreatedAt, CreatedBy: mn.CreatedBy, UpdatedAt: mn.UpdatedAt, UpdatedBy: mn.UpdatedBy,
				Error: mn.Error, UsedBy: nonNil(mn.UsedBy),
			}
		}

		writeJSON(w, out)
	}
}

// maskWebhook returns wh with every header value replaced by null (see
// webhookConfigJSON).
func maskWebhook(wh *notify.FileWebhook) *webhookConfigJSON {
	if wh == nil {
		return nil
	}

	headers := make(map[string]*string, len(wh.Headers))
	for name := range wh.Headers {
		headers[name] = nil
	}

	return &webhookConfigJSON{URL: wh.URL, Method: wh.Method, Headers: headers, Body: wh.Body}
}

// handleCreateNotificationConfig serves POST /api/notification-configs
// (admin only), answering 201 on success.
func handleCreateNotificationConfig(m *settings.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in settings.NotificationInput
		if !decodeSettingsBody(w, r, &in) {
			return
		}

		user, _ := currentUser(r.Context())

		if err := m.CreateNotification(r.Context(), user.Username, in); err != nil {
			writeSettingsError(w, log, "creating notification", in.ID, err)
			return
		}

		w.WriteHeader(http.StatusCreated)
	}
}

// handleUpdateNotificationConfig serves PUT /api/notification-configs/{id}
// (admin only). The id is taken from the URL; it can't be changed.
func handleUpdateNotificationConfig(m *settings.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in settings.NotificationInput
		if !decodeSettingsBody(w, r, &in) {
			return
		}

		id := r.PathValue("id")
		if in.ID != "" && in.ID != id {
			http.Error(w, "a notification's id can't be changed", http.StatusBadRequest)
			return
		}

		in.ID = id
		user, _ := currentUser(r.Context())

		if err := m.UpdateNotification(r.Context(), user.Username, in); err != nil {
			writeSettingsError(w, log, "updating notification", id, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// handleDeleteNotificationConfig serves DELETE
// /api/notification-configs/{id} (admin only), answering 409 while it's
// still referenced.
func handleDeleteNotificationConfig(m *settings.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		user, _ := currentUser(r.Context())

		if err := m.DeleteNotification(r.Context(), user.Username, id); err != nil {
			writeSettingsError(w, log, "deleting notification", id, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// reportConfigJSON is GET /api/report-config's response: the stored report
// settings as entered, plus what the dashboard's form needs.
type reportConfigJSON struct {
	report.FileReport

	UpdatedAt       *time.Time `json:"updated_at"`
	UpdatedBy       string     `json:"updated_by"`
	Error           string     `json:"error,omitempty"`
	DefaultSchedule string     `json:"default_schedule"`
	NextRun         *time.Time `json:"next_run"`
	NotificationIDs []string   `json:"notification_ids"`
}

// handleGetReportConfig serves GET /api/report-config (admin only).
func handleGetReportConfig(m *settings.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mr, err := m.GetReport(r.Context())
		if err != nil {
			writeSettingsError(w, log, "reading report settings", "", err)
			return
		}

		out := reportConfigJSON{
			Enabled: mr.Enabled, Schedule: mr.Schedule, Notifications: nonNil(mr.Notifications),
			UpdatedBy: mr.UpdatedBy, Error: mr.Error,
			DefaultSchedule: report.DefaultSchedule, NotificationIDs: nonNil(m.NotificationIDs()),
		}

		if !mr.UpdatedAt.IsZero() {
			out.UpdatedAt = &mr.UpdatedAt
		}

		if next, ok := m.NextReport(time.Now()); ok {
			out.NextRun = &next
		}

		writeJSON(w, out)
	}
}

// handleUpdateReportConfig serves PUT /api/report-config (admin only).
func handleUpdateReportConfig(m *settings.Manager, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in report.FileReport
		if !decodeSettingsBody(w, r, &in) {
			return
		}

		user, _ := currentUser(r.Context())

		if err := m.UpdateReport(r.Context(), user.Username, in); err != nil {
			writeSettingsError(w, log, "updating report settings", "", err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeSettingsBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBody)).Decode(v); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}

	return true
}

// writeSettingsError maps a settings.Manager error onto its HTTP status,
// with validation and in-use messages passed through for the dashboard to
// show.
func writeSettingsError(w http.ResponseWriter, log *slog.Logger, action, id string, err error) {
	switch {
	case errors.Is(err, settings.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrNotificationNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrNotificationExists), errors.Is(err, settings.ErrNotificationInUse):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, settings.ErrStoreUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		log.Warn("web UI: "+action+" failed", "id", id, "err", err)
		http.Error(w, action+" failed", http.StatusInternalServerError)
	}
}
