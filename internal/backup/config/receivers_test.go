package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

// testReceiverPublicKeyPEM generates a fresh (test-speed) RSA key pair and
// returns its public key both PEM-encoded (as a receivers: entry's
// public-key: value) and parsed (to build the resolvedReceiver a test
// expects buildReceivers to produce).
func testReceiverPublicKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshaling public key: %v", err)
	}

	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	return pemText, &key.PublicKey
}

func TestBuildReceivers(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]FileReceiver{
		{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a"},
		{ID: "b", AllowedServers: []string{testSenderID}, Path: "/mnt/b", Retention: "7d"},
	}, nil, "")
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := map[string]ResolvedReceiver{
		"a": {ID: "a", Kind: ReceiverKindBackup, AllowedServers: []string{testSenderID}, Path: "/mnt/a", MaxObjectSize: DefaultReceiverMaxObjectSize},
		"b": {ID: "b", Kind: ReceiverKindBackup, AllowedServers: []string{testSenderID}, Path: "/mnt/b", Retention: 7 * 24 * time.Hour, MaxObjectSize: DefaultReceiverMaxObjectSize},
	}

	if len(receivers) != len(want) {
		t.Fatalf("buildReceivers() = %+v, want %+v", receivers, want)
	}

	for id, w := range want {
		if got := receivers[id]; !reflect.DeepEqual(got, w) {
			t.Errorf("buildReceivers()[%q] = %+v, want %+v", id, got, w)
		}
	}
}

func TestBuildReceiversRequiresID(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{{AllowedServers: []string{testSenderID}, Path: "/mnt/a"}}, nil, "")
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing id, got nil")
	}
}

// testSenderID is the trusted server every test receiver allows. With no
// trust registry, ResolveReceiver accepts any id, so it needn't exist.
const testSenderID = "6f1c2a4e-8d3b-4c5a-9e7f-0a1b2c3d4e5f"

func TestBuildReceiversRequiresAllowedServer(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{{ID: "a", Path: "/mnt/a"}}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "an allowed server is required") {
		t.Fatalf("buildReceivers() error = %v, want an allowed server to be required", err)
	}
}

func TestBuildReceiversRequiresPath(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{{ID: "a", AllowedServers: []string{testSenderID}}}, nil, "")
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing path, got nil")
	}
}

func TestParseStaleAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty disables it", in: "", want: 0},
		{name: "hours", in: "6h", want: 6 * time.Hour},
		{name: "days", in: "1d", want: 24 * time.Hour},
		{name: "zero is not meaningful", in: "0", wantErr: true},
		{name: "negative is not meaningful", in: "-1h", wantErr: true},
		{name: "garbage", in: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseStaleAfter(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseStaleAfter(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}

			if err == nil && got != tt.want {
				t.Errorf("parseStaleAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// testNotification builds a single-webhook notify.Notification for a test
// fixture's notifications map.
func testNotification(id, url string) notify.Notification {
	return notify.Notification{ID: id, Webhook: &notify.Webhook{URL: url, Method: "POST"}}
}

func TestBuildReceiversStaleAfterAndNotifications(t *testing.T) {
	t.Parallel()

	pagerduty := testNotification("pagerduty", "https://example.com/hook")
	notifications := notify.NewRegistry(map[string]notify.Notification{"pagerduty": pagerduty})

	receivers, err := buildReceivers([]FileReceiver{
		{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", StaleAfter: "6h", StaleNotifications: []string{"pagerduty"}},
	}, notifications, "")
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := ResolvedReceiver{
		ID: "a", Kind: ReceiverKindBackup, AllowedServers: []string{testSenderID}, Path: "/mnt/a",
		MaxObjectSize:      DefaultReceiverMaxObjectSize,
		StaleAfter:         6 * time.Hour,
		StaleNotifications: []string{"pagerduty"},
		Notifications:      notifications,
	}

	if got := receivers["a"]; !reflect.DeepEqual(got, want) {
		t.Errorf("buildReceivers()[%q] = %+v, want %+v", "a", got, want)
	}
}

func TestBuildReceiversStaleAfterRequiresNotifications(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", StaleAfter: "6h"}}, nil, "")
	if err == nil {
		t.Fatal("buildReceivers() expected error for stale-after without stale-notifications, got nil")
	}
}

func TestBuildReceiversNotificationsRequireStaleAfter(t *testing.T) {
	t.Parallel()

	pagerduty := testNotification("pagerduty", "https://example.com/hook")
	notifications := notify.NewRegistry(map[string]notify.Notification{"pagerduty": pagerduty})

	_, err := buildReceivers([]FileReceiver{
		{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", StaleNotifications: []string{"pagerduty"}},
	}, notifications, "")
	if err == nil {
		t.Fatal("buildReceivers() expected error for stale-notifications without stale-after, got nil")
	}
}

func TestBuildReceiversUnknownStaleNotificationID(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{
		{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", StaleAfter: "6h", StaleNotifications: []string{"nope"}},
	}, notify.NewRegistry(nil), "")
	if err == nil {
		t.Fatal("buildReceivers() expected error for unknown stale-notifications id, got nil")
	}
}

func TestBuildReceiversDownloadNotificationsIndependentOfStaleAfter(t *testing.T) {
	t.Parallel()

	slack := testNotification("slack", "https://example.com/downloaded")
	notifications := notify.NewRegistry(map[string]notify.Notification{"slack": slack})

	receivers, err := buildReceivers([]FileReceiver{
		{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", DownloadNotifications: []string{"slack"}},
	}, notifications, "")
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := ResolvedReceiver{
		ID: "a", Kind: ReceiverKindBackup, AllowedServers: []string{testSenderID}, Path: "/mnt/a",
		MaxObjectSize:         DefaultReceiverMaxObjectSize,
		DownloadNotifications: []string{"slack"},
		Notifications:         notifications,
	}

	if got := receivers["a"]; !reflect.DeepEqual(got, want) {
		t.Errorf("buildReceivers()[%q] = %+v, want %+v", "a", got, want)
	}
}

func TestBuildReceiversDownloadAndStaleNotificationsCoexist(t *testing.T) {
	t.Parallel()

	pagerduty := testNotification("pagerduty", "https://example.com/stale")
	slack := testNotification("slack", "https://example.com/downloaded")
	notifications := notify.NewRegistry(map[string]notify.Notification{"pagerduty": pagerduty, "slack": slack})

	receivers, err := buildReceivers([]FileReceiver{
		{
			ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a",
			StaleAfter:            "6h",
			StaleNotifications:    []string{"pagerduty"},
			DownloadNotifications: []string{"slack"},
		},
	}, notifications, "")
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	got := receivers["a"]
	if !reflect.DeepEqual(got.StaleNotifications, []string{"pagerduty"}) {
		t.Errorf("StaleNotifications = %+v, want [pagerduty]", got.StaleNotifications)
	}

	if !reflect.DeepEqual(got.DownloadNotifications, []string{"slack"}) {
		t.Errorf("DownloadNotifications = %+v, want [slack]", got.DownloadNotifications)
	}
}

func TestBuildReceiversUnknownDownloadNotificationID(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{
		{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", DownloadNotifications: []string{"nope"}},
	}, notify.NewRegistry(nil), "")
	if err == nil {
		t.Fatal("buildReceivers() expected error for unknown download-notifications id, got nil")
	}
}

func TestBuildReceiversNoNotificationsResolveToNil(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]FileReceiver{{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a"}}, nil, "")
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	if got := receivers["a"].DownloadNotifications; got != nil {
		t.Errorf("DownloadNotifications = %+v, want nil when unset", got)
	}

	if got := receivers["a"].StaleNotifications; got != nil {
		t.Errorf("StaleNotifications = %+v, want nil when unset", got)
	}
}

func TestBuildReceiversServerNamePropagatesToNotificationPlaceholder(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]FileReceiver{{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a"}}, nil, "primary-backup-host")
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	if got := receivers["a"].ServerName; got != "primary-backup-host" {
		t.Errorf("ServerName = %q, want %q", got, "primary-backup-host")
	}
}

func TestParseRetention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty means no expiry", in: "", want: 0},
		{name: "hours", in: "168h", want: 168 * time.Hour},
		{name: "days", in: "7d", want: 7 * 24 * time.Hour},
		{name: "days plus hours", in: "1d12h", want: 36 * time.Hour},
		{name: "fractional days", in: "1.5d", want: 36 * time.Hour},
		{name: "minutes", in: "90m", want: 90 * time.Minute},
		{name: "negative hours rejected", in: "-1h", wantErr: true},
		{name: "negative days rejected", in: "-1d", wantErr: true},
		{name: "bare sign rejected", in: "-", wantErr: true},
		{name: "unit with no number rejected", in: "d", wantErr: true},
		{name: "garbage rejected", in: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseRetention(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseRetention(%q) error = nil, want error", tt.in)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("parseRetention(%q) = (%v, %v), want (%v, nil)", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestValidateReceiverPath(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	outside := t.TempDir()

	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	tests := []struct {
		name    string
		baseDir string
		path    string
		wantErr bool
	}{
		{"direct child", base, filepath.Join(base, "a"), false},
		{"nested, not yet existing", base, filepath.Join(base, "a", "b", "c"), false},
		{"base itself", base, base, true},
		{"dot-dot escape", base, filepath.Join(base, "..", "elsewhere"), true},
		{"sibling with shared prefix", base, base + "-other", true},
		{"relative", base, "a/b", true},
		{"symlink escape", base, filepath.Join(base, "escape", "x"), true},
		{"no base dir", "", filepath.Join(base, "a"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateReceiverPath(tt.baseDir, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateReceiverPath(%q, %q) error = %v, wantErr %v", tt.baseDir, tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestResolveReceiverRequiresID(t *testing.T) {
	t.Parallel()

	if _, err := ResolveReceiver(FileReceiver{ID: "  ", AllowedServers: []string{testSenderID}, Path: "/mnt/a"}, nil, nil, ""); err == nil {
		t.Fatal("ResolveReceiver() expected error for blank id, got nil")
	}
}

func TestResolveReceiverAllowedServers(t *testing.T) {
	t.Parallel()

	const id = "0b8e1d5c-5a4e-4f43-9d6b-1d2a3b4c5d6e"

	_, pub := testReceiverPublicKeyPEM(t)
	trusted := trust.NewRegistry(map[string]trust.Server{id: {ID: id, Name: "nas", PublicKey: pub}})

	// Allowed servers alone suffice; ids are normalized and deduplicated.
	recv, err := ResolveReceiver(FileReceiver{ID: "a", AllowedServers: []string{strings.ToUpper(id), id}, Path: "/mnt/a"}, nil, trusted, "")
	if err != nil {
		t.Fatalf("ResolveReceiver() error: %v", err)
	}

	if !reflect.DeepEqual(recv.AllowedServers, []string{id}) || recv.TrustedServers != trusted {
		t.Errorf("ResolveReceiver() = %+v, want AllowedServers [%s], no public key, the trusted registry", recv, id)
	}

	cases := map[string]FileReceiver{
		"unknown server":       {ID: "a", AllowedServers: []string{"6f1c0e0a-0000-4000-8000-000000000000"}, Path: "/mnt/a"},
		"neither key nor list": {ID: "a", Path: "/mnt/a"},
		"empty id in list":     {ID: "a", AllowedServers: []string{" "}, Path: "/mnt/a"},
	}

	for name, fr := range cases {
		if _, err := ResolveReceiver(fr, nil, trusted, ""); err == nil {
			t.Errorf("%s: ResolveReceiver() = nil error, want one", name)
		}
	}
}

// buildReceivers is a test harness for ResolveReceiver: it resolves
// fileReceivers into an id -> ResolvedReceiver map, requiring every entry to
// have a unique, non-empty id. notifications, if non-nil, is checked for
// every stale-notifications:/download-notifications: id, and serverName is
// copied onto every receiver (see ResolveReceiver).
func buildReceivers(fileReceivers []FileReceiver, notifications *notify.Registry, serverName string) (map[string]ResolvedReceiver, error) {
	receivers := make(map[string]ResolvedReceiver, len(fileReceivers))

	for i, fr := range fileReceivers {
		id := strings.TrimSpace(fr.ID)
		if id == "" {
			return nil, fmt.Errorf("receivers[%d]: id is required", i)
		}

		if _, exists := receivers[id]; exists {
			return nil, fmt.Errorf("receivers[%d]: duplicate receiver id %q", i, id)
		}

		recv, err := ResolveReceiver(fr, notifications, nil, serverName)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		receivers[id] = recv
	}

	return receivers, nil
}

func TestResolveReceiverImmutable(t *testing.T) {
	t.Parallel()

	backup := FileReceiver{ID: "a", AllowedServers: []string{testSenderID}, Path: "/mnt/a", Immutable: true}
	if got, err := ResolveReceiver(backup, nil, nil, ""); err != nil || !got.Immutable {
		t.Errorf("immutable backup receiver = (Immutable %v, err %v), want (true, nil)", got.Immutable, err)
	}

	directory := backup
	directory.Type = string(ReceiverKindDirectory)

	if _, err := ResolveReceiver(directory, nil, nil, ""); err == nil {
		t.Error("immutable directory receiver = nil error, want one")
	}
}
