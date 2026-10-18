package webui

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/reporter"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// startReportsWebUI starts an SSO-enabled web UI whose reports are kept in
// a fresh state db, generated for one job "nightly".
func startReportsWebUI(t *testing.T, idp *testIDP) *Server {
	t.Helper()

	db := openTestStateDB(t)
	rep := reporter.New(reporter.Deps{
		JobNames:  func() []string { return []string{"nightly"} },
		Settings:  report.NewLive(report.Settings{}),
		Receivers: func() map[string]config.ResolvedReceiver { return nil },
		DB:        db,
		Log:       discardLogger,
	})
	statusStore, _ := newTestStore()

	srv := StartWebUI(Deps{Log: discardLogger, DB: db, Status: statusStore, Reporter: rep}, Options{
		Addr: "127.0.0.1:0",
		OIDC: idp.settings(),
	})
	if srv == nil {
		t.Fatal("StartWebUI() = nil, want a running server")
	}

	t.Cleanup(srv.Shutdown)

	return srv
}

// TestReportsAPI checks that only an admin can generate and read reports,
// and that a generated report is listed and readable with its content.
func TestReportsAPI(t *testing.T) {
	t.Parallel()

	idp := newTestIDP(t)
	srv := startReportsWebUI(t, idp)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)

	for _, c := range []receiverConfigCall{
		{"non-admin list", http.MethodGet, "/api/reports", viewer, nil, http.StatusForbidden},
		{"non-admin generate", http.MethodPost, "/api/reports", viewer, generateReportInput{}, http.StatusForbidden},
		{"negative period", http.MethodPost, "/api/reports", admin, generateReportInput{Hours: -1}, http.StatusBadRequest},
		{"period too long", http.MethodPost, "/api/reports", admin, generateReportInput{Hours: 24*90 + 1}, http.StatusBadRequest},
		{"bad id", http.MethodGet, "/api/reports/nope", admin, nil, http.StatusBadRequest},
		{"missing", http.MethodGet, "/api/reports/42", admin, nil, http.StatusNotFound},
	} {
		c.run(t, srv)
	}

	var generated reportJSON
	if got := postJSON(t, srv, "/api/reports", admin, generateReportInput{Hours: 6}, &generated); got != http.StatusCreated {
		t.Fatalf("POST /api/reports = %d, want %d", got, http.StatusCreated)
	}

	if generated.Trigger != store.ReportTriggerManual || generated.TriggeredBy != "erin" ||
		generated.PeriodEnd.Sub(generated.PeriodStart) != 6*time.Hour || len(generated.Content.Jobs) != 1 {
		t.Errorf("POST /api/reports = %+v, want a manual 6h report by erin covering nightly", generated)
	}

	var list []reportSummaryJSON
	if got := doJSON(t, srv, http.MethodGet, "/api/reports", admin, &list); got != http.StatusOK || len(list) != 1 || list[0].ID != generated.ID {
		t.Fatalf("GET /api/reports = %d %+v, want the generated report", got, list)
	}

	var got reportJSON
	if code := doJSON(t, srv, http.MethodGet, "/api/reports/"+strconv.FormatUint(uint64(generated.ID), 10), admin, &got); code != http.StatusOK || got.Jobs != 1 || got.Content.Jobs[0].ID != "nightly" {
		t.Errorf("GET /api/reports/{id} = %d %+v, want the generated report", code, got)
	}
}
