package receiver

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
)

func TestImmutableReceiver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		immutable bool
		second    string
		wantErr   string // substring of the second upload's error; "" for success
		wantBody  string
	}{
		{name: "mutable receiver overwrites", second: "changed", wantBody: "changed"},
		{name: "immutable receiver accepts an identical upload", immutable: true, second: "original", wantBody: "original"},
		{name: "immutable receiver refuses an overwrite", immutable: true, second: "changed", wantErr: "409", wantBody: "original"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			id, key := testServerIdentityAndKey(t)
			recv := config.ResolvedReceiver{
				ID: "r", AllowedServers: []string{testSenderUUID}, TrustedServers: trustSender(&key.PublicKey), Path: dir,
				Immutable: tt.immutable,
			}

			srv := httptest.NewServer(newReceiverMux(map[string]config.ResolvedReceiver{recv.ID: recv}))
			defer srv.Close()

			tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: recv.ID}
			cfg := &config.Config{Key: "db.gpg", CreatedAt: time.Now().Add(-time.Hour)}

			if err := remoteclient.Upload(t.Context(), id, cfg, tgt, strings.NewReader("original")); err != nil {
				t.Fatalf("first Upload() unexpected error: %v", err)
			}

			err := remoteclient.Upload(t.Context(), id, cfg, tgt, strings.NewReader(tt.second))

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("second Upload() unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("second Upload() error = %v, want one mentioning %s", err, tt.wantErr)
			}

			if got, err := os.ReadFile(filepath.Join(dir, "db.gpg")); err != nil || string(got) != tt.wantBody { //nolint:gosec // dir is t.TempDir()
				t.Errorf("stored object = (%q, %v), want %q", got, err, tt.wantBody)
			}
		})
	}
}
