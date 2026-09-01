package app

import (
	"io"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// TestNewRunLoggerLogViewerGate covers newRunLogger's *backup.LogRingBuffer
// decision: it must only wire one up (so /api/logs has anything to serve)
// when both the web UI is enabled (listen:) and the operator opted into the
// log viewer (enable-log-viewer:) — never from listen: alone, since unlike
// file downloads the dashboard has no login guarding the viewer.
func TestNewRunLoggerLogViewerGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		listen    string
		logViewer bool
		wantLogs  bool
	}{
		{name: "no web UI, viewer unset", listen: "", logViewer: false, wantLogs: false},
		{name: "no web UI, viewer enabled", listen: "", logViewer: true, wantLogs: false},
		{name: "web UI, viewer unset", listen: ":0", logViewer: false, wantLogs: false},
		{name: "web UI, viewer enabled", listen: ":0", logViewer: true, wantLogs: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rc := &config.RunConfig{Listen: tt.listen, LogViewer: tt.logViewer}

			_, logs := newRunLogger(io.Discard, rc)
			if got := logs != nil; got != tt.wantLogs {
				t.Errorf("newRunLogger() logs != nil = %v, want %v", got, tt.wantLogs)
			}
		})
	}
}
