package webui

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/reporter"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// reportListLimit caps how many reports GET /api/reports returns.
const reportListLimit = 500

// defaultGenerateHours is POST /api/reports' period when hours is unset.
const defaultGenerateHours = 24

// reportSummaryJSON is one kept report as GET /api/reports lists it.
type reportSummaryJSON struct {
	ID             uint      `json:"id"`
	GeneratedAt    time.Time `json:"generated_at"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	Trigger        string    `json:"trigger"`
	TriggeredBy    string    `json:"triggered_by"`
	Notifications  []string  `json:"notifications"`
	Receivers      int       `json:"receivers"`
	ReceiverErrors int       `json:"receiver_errors"`
	Stale          int       `json:"stale"`
	Jobs           int       `json:"jobs"`
	JobErrors      int       `json:"job_errors"`
}

// reportJSON is one kept report with its content, as GET /api/reports/{id}
// and POST /api/reports return it. The content's own JSON shape (see
// store.ReportContent) is served as is.
type reportJSON struct {
	reportSummaryJSON

	Content store.ReportContent `json:"content"`
}

func reportSummaryToJSON(r store.ReportSummary) reportSummaryJSON {
	return reportSummaryJSON{
		ID: r.ID, GeneratedAt: r.GeneratedAt, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd,
		Trigger: r.Trigger, TriggeredBy: r.TriggeredBy, Notifications: nonNil(r.Notifications),
		Receivers: r.Receivers, ReceiverErrors: r.ReceiverErrors, Stale: r.Stale, Jobs: r.Jobs, JobErrors: r.JobErrors,
	}
}

func reportToJSON(r store.Report) reportJSON {
	c := r.Content
	c.Receivers = nonNil(c.Receivers)
	c.Stale = nonNil(c.Stale)
	c.ReceiverErrors = nonNil(c.ReceiverErrors)
	c.Jobs = nonNil(c.Jobs)
	c.JobErrors = nonNil(c.JobErrors)

	return reportJSON{reportSummaryJSON: reportSummaryToJSON(r.ReportSummary), Content: c}
}

// registerReportRoutes mounts /api/reports on mux: listing and reading
// kept reports wrapped in admin, generating one in change. Without a
// Reporter only the reading routes exist.
func registerReportRoutes(mux *http.ServeMux, db *store.Store, rep *reporter.Reporter, admin, change func(http.HandlerFunc) http.HandlerFunc, log *slog.Logger) {
	mux.HandleFunc("GET /api/reports", admin(handleListReports(db, log)))
	mux.HandleFunc("GET /api/reports/{id}", admin(handleGetReport(db, log)))

	if rep != nil {
		mux.HandleFunc("POST /api/reports", change(handleGenerateReport(rep, log)))
	}
}

// handleListReports serves GET /api/reports (admin only): every kept report,
// newest first, without content.
func handleListReports(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, reporter.ErrHistoryUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}

		list, err := db.ListReports(r.Context(), reportListLimit)
		if err != nil {
			log.Warn("web UI: listing reports failed", "err", err)
			http.Error(w, "listing reports failed", http.StatusInternalServerError)

			return
		}

		out := make([]reportSummaryJSON, len(list))
		for i, rs := range list {
			out[i] = reportSummaryToJSON(rs)
		}

		writeJSON(w, out)
	}
}

// handleGetReport serves GET /api/reports/{id} (admin only).
func handleGetReport(db *store.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, reporter.ErrHistoryUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}

		id, err := strconv.ParseUint(r.PathValue("id"), 10, 0)
		if err != nil {
			http.Error(w, "invalid report id", http.StatusBadRequest)
			return
		}

		rep, err := db.GetReport(r.Context(), uint(id))
		if err != nil {
			if errors.Is(err, store.ErrReportNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}

			log.Warn("web UI: reading report failed", "id", id, "err", err)
			http.Error(w, "reading report failed", http.StatusInternalServerError)

			return
		}

		writeJSON(w, reportToJSON(rep))
	}
}

// generateReportInput is POST /api/reports' body.
type generateReportInput struct {
	// Hours is how far back the report reaches from now; 0 means
	// defaultGenerateHours.
	Hours int `json:"hours"`
}

// handleGenerateReport serves POST /api/reports (admin only): builds a
// report up to now, keeps it, and returns it. It isn't sent anywhere.
func handleGenerateReport(rep *reporter.Reporter, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in generateReportInput
		if !decodeSettingsBody(w, r, &in) {
			return
		}

		if in.Hours == 0 {
			in.Hours = defaultGenerateHours
		}

		period := time.Duration(in.Hours) * time.Hour
		if in.Hours < 0 || period > reporter.MaxGeneratePeriod {
			http.Error(w, "hours must be between 1 and "+strconv.Itoa(int(reporter.MaxGeneratePeriod/time.Hour)), http.StatusBadRequest)
			return
		}

		user, _ := currentUser(r.Context())

		kept, err := rep.Generate(r.Context(), user.Username, period)
		if err != nil {
			if errors.Is(err, reporter.ErrHistoryUnavailable) {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}

			log.Warn("web UI: generating report failed", "err", err)
			http.Error(w, "generating report failed", http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, reportToJSON(kept))
	}
}
