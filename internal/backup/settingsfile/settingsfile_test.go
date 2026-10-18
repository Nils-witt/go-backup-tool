package settingsfile

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/receiver"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/settings"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

var discardLogger = slog.New(slog.DiscardHandler)

const senderID = "6f1c2a4e-8d3b-4c5a-9e7f-0a1b2c3d4e5f"

// newManagers builds the managers the way the app does, on a fresh state db,
// with receiver paths restricted to baseDir.
func newManagers(t *testing.T, baseDir string) Managers {
	t.Helper()

	db, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.OpenSQLite() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	notifications := notify.NewRegistry(nil)
	sm := settings.NewManager(db, notifications, report.NewLive(report.Settings{}), notify.SMTPSettings{}, notify.GPGSettings{}, discardLogger)
	sm.Load(t.Context())

	tm := trust.NewManager(db, trust.NewRegistry(nil), discardLogger)
	tm.Load(t.Context())

	rm := receiver.NewManager(db, receiver.NewRegistry(nil), status.NewReceiverStore(nil), notifications, tm.Registry(), "host", baseDir, discardLogger)
	rm.Load(t.Context())

	return Managers{Trust: tm, Settings: sm, Receivers: rm}
}

func testPublicKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshaling public key: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// sampleFile uses every section, each referring to the ones before it.
func sampleFile(t *testing.T, baseDir string) File {
	t.Helper()

	return File{
		TrustedServers: []TrustedServer{{ID: senderID, Name: "primary", PublicKey: testPublicKeyPEM(t)}},
		Notifications: []notify.FileNotification{{
			ID:      "ops",
			Webhook: &notify.FileWebhook{URL: "https://hooks.example/x", Method: "POST", Headers: map[string]string{"Authorization": "Bearer t"}, Body: "{job}"},
		}},
		Receivers: []config.FileReceiver{{
			ID: "from-primary", AllowedServers: []string{senderID}, Path: filepath.Join(baseDir, "from-primary"), Retention: "30d",
			StaleAfter: "26h", StaleNotifications: []string{"ops"},
		}},
		Report: &report.FileReport{Enabled: true, Schedule: "0 6 * * *", Notifications: []string{"ops"}},
	}
}

func TestImportThenExportRoundTrips(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	m := newManagers(t, baseDir)
	in := sampleFile(t, baseDir)

	res, err := Import(t.Context(), m, "test", in)
	if err != nil {
		t.Fatalf("Import() error: %v", err)
	}

	expected := map[string]int{"trusted-servers": 1, "notifications": 1, "receivers": 1}
	if !reflect.DeepEqual(res.Created, expected) || !reflect.DeepEqual(res.Updated, map[string]int{"report": 1}) {
		t.Errorf("Import() = %+v, want created %v and the report updated", res, expected)
	}

	got, err := Export(t.Context(), m)
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}

	assertExportedSample(t, got, in)

	// Importing the export into a fresh instance reproduces it exactly.
	other := newManagers(t, baseDir)
	if _, err := Import(t.Context(), other, "test", got); err != nil {
		t.Fatalf("Import() of the export into a fresh instance error: %v", err)
	}

	again, err := Export(t.Context(), other)
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}

	if !reflect.DeepEqual(again, got) {
		t.Errorf("re-exported settings differ:\n got %+v\nwant %+v", again, got)
	}
}

// assertExportedSample checks that got, an export after importing in (see
// sampleFile), holds each of in's entries.
func assertExportedSample(t *testing.T, got, in File) {
	t.Helper()

	if len(got.TrustedServers) != 1 || got.TrustedServers[0].ID != senderID || got.TrustedServers[0].Name != "primary" {
		t.Errorf("exported trusted servers = %+v", got.TrustedServers)
	}

	if len(got.Notifications) != 1 || !reflect.DeepEqual(got.Notifications[0].Webhook, in.Notifications[0].Webhook) {
		t.Errorf("exported notifications = %+v, want the webhook with its header value", got.Notifications)
	}

	if len(got.Receivers) != 1 || got.Receivers[0].ID != "from-primary" || !reflect.DeepEqual(got.Receivers[0].AllowedServers, []string{senderID}) {
		t.Errorf("exported receivers = %+v", got.Receivers)
	}

	if !reflect.DeepEqual(got.Report, in.Report) {
		t.Errorf("exported report = %+v, want %+v", got.Report, in.Report)
	}
}

func TestImportTwiceUpdatesInsteadOfDuplicating(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	m := newManagers(t, baseDir)
	in := sampleFile(t, baseDir)

	if _, err := Import(t.Context(), m, "test", in); err != nil {
		t.Fatalf("first Import() error: %v", err)
	}

	in.Receivers[0].Retention = "7d"

	res, err := Import(t.Context(), m, "test", in)
	if err != nil {
		t.Fatalf("second Import() error: %v", err)
	}

	if len(res.Created) != 0 || res.Updated["receivers"] != 1 || res.Updated["notifications"] != 1 || res.Updated["trusted-servers"] != 1 {
		t.Errorf("second Import() = %+v, want everything updated and nothing created", res)
	}

	got, err := Export(t.Context(), m)
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}

	if len(got.Receivers) != 1 || got.Receivers[0].Retention != "7d" {
		t.Errorf("exported receivers = %+v, want the one receiver with retention 7d", got.Receivers)
	}
}

func TestImportStopsAtTheFirstInvalidEntry(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	m := newManagers(t, baseDir)
	in := sampleFile(t, baseDir)
	in.Receivers[0].AllowedServers = []string{"00000000-0000-0000-0000-000000000000"}

	_, err := Import(t.Context(), m, "test", in)
	if err == nil || !strings.Contains(err.Error(), `receiver "from-primary"`) {
		t.Fatalf("Import() error = %v, want it to name the invalid receiver", err)
	}

	got, err := Export(t.Context(), m)
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}

	if len(got.TrustedServers) != 1 || len(got.Notifications) != 1 || len(got.Receivers) != 0 || got.Report != nil {
		t.Errorf("after a failed import got %+v, want the entries before the invalid one applied and nothing after", got)
	}
}

func TestDecodeRejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	if _, err := Decode(strings.NewReader("receivers: []\nunknown-section: oops\n")); err == nil {
		t.Fatal("Decode() of a file with an unknown key succeeded, want an error")
	}

	nested := "notifications:\n  - id: n\n    email:\n      to: [ops@example.com]\n      encrypt:\n        recipients: [ops@example.com]\n        gpg-bin: /usr/local/bin/gpg\n"
	if _, err := Decode(strings.NewReader(nested)); err == nil || !strings.Contains(err.Error(), "gpg-bin") {
		t.Fatalf("Decode() with a removed email.encrypt gpg-bin error = %v, want it to mention gpg-bin", err)
	}

	f, err := Decode(strings.NewReader(""))
	if err != nil || !reflect.DeepEqual(f, File{}) {
		t.Errorf("Decode() of an empty file = %+v, %v, want an empty File", f, err)
	}
}

func TestExportFileIsOwnerOnlyAndDecodes(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	m := newManagers(t, baseDir)

	if _, err := Import(t.Context(), m, "test", sampleFile(t, baseDir)); err != nil {
		t.Fatalf("Import() error: %v", err)
	}

	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := ExportFile(t.Context(), m, path); err != nil {
		t.Fatalf("ExportFile() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("exported file mode = %v, want 0600", info.Mode().Perm())
	}

	res, err := ImportFile(t.Context(), m, "test", path)
	if err != nil {
		t.Fatalf("ImportFile() of the exported file error: %v", err)
	}

	if len(res.Created) != 0 {
		t.Errorf("re-importing the export created %v, want only updates", res.Created)
	}
}
