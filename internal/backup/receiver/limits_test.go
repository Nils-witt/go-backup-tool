package receiver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
)

func TestReceiveObjectUploadLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		max      int64
		quota    int64
		existing map[string]int // files already under the receiver path, by key
		key      string
		body     string
		wantErr  string // substring of remoteclient.Upload's error; "" for success
	}{
		{name: "within max size", max: 10, key: "a.gpg", body: "0123456789"},
		{name: "over max size", max: 9, key: "a.gpg", body: "0123456789", wantErr: "413"},
		{name: "within quota", max: 100, quota: 15, existing: map[string]int{"old.gpg": 5}, key: "a.gpg", body: "0123456789"},
		{name: "over quota", max: 100, quota: 14, existing: map[string]int{"old.gpg": 5}, key: "a.gpg", body: "0123456789", wantErr: "507"},
		{name: "quota full", max: 100, quota: 5, existing: map[string]int{"old.gpg": 5}, key: "a.gpg", body: "x", wantErr: "507"},
		{name: "replacing frees its own size", max: 100, quota: 10, existing: map[string]int{"a.gpg": 8}, key: "a.gpg", body: "0123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for key, size := range tt.existing {
				if err := os.WriteFile(filepath.Join(dir, key), make([]byte, size), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			id, key := testServerIdentityAndKey(t)
			recv := config.ResolvedReceiver{
				ID: "r", AllowedServers: []string{testSenderUUID}, TrustedServers: trustSender(&key.PublicKey), Path: dir,
				MaxObjectSize: tt.max, Quota: tt.quota,
			}

			srv := httptest.NewServer(newReceiverMux(map[string]config.ResolvedReceiver{recv.ID: recv}))
			defer srv.Close()

			tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: recv.ID}
			err := remoteclient.Upload(t.Context(), id, &config.Config{Key: tt.key}, tgt, strings.NewReader(tt.body))

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Upload() unexpected error: %v", err)
				}

				if got, err := os.ReadFile(filepath.Join(dir, tt.key)); err != nil || string(got) != tt.body { //nolint:gosec // dir is t.TempDir()
					t.Errorf("stored %q = (%q, %v), want the uploaded body", tt.key, got, err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Upload() error = %v, want one mentioning %s", err, tt.wantErr)
			}

			if _, err := os.Stat(filepath.Join(dir, "a.gpg")); tt.existing["a.gpg"] == 0 && err == nil {
				t.Error("rejected upload was stored anyway")
			}
		})
	}
}

// TestLimitUploadBodyWithoutContentLength checks a body over the limit is
// cut off while streaming when no Content-Length announced it.
func TestLimitUploadBodyWithoutContentLength(t *testing.T) {
	t.Parallel()

	recv := config.ResolvedReceiver{ID: "r", Path: t.TempDir(), MaxObjectSize: 4}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/", strings.NewReader("0123456789"))
	req.ContentLength = -1

	rec := httptest.NewRecorder()

	body, limit, ok := limitUploadBody(rec, req, recv, "", discardLogger)
	if !ok {
		t.Fatalf("limitUploadBody() rejected a body of unknown length up front: %d", rec.Code)
	}

	buf := make([]byte, 16)

	var readErr error
	for readErr == nil {
		_, readErr = body.Read(buf)
	}

	if !writeUploadLimitError(rec, readErr, limit) {
		t.Fatalf("read error %v isn't recognized as over the limit", readErr)
	}

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}
