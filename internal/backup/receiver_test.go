package backup

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
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

func TestSanitizeObjectKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key     string
		wantErr bool
	}{
		{key: "backup-20260101.gpg", wantErr: false},
		{key: "subdir/backup-20260101.gpg", wantErr: false},
		{key: "", wantErr: true},
		{key: "/etc/passwd", wantErr: true},
		{key: "..", wantErr: true},
		{key: "../etc/passwd", wantErr: true},
		{key: "subdir/../../etc/passwd", wantErr: true},
		{key: "subdir//double-slash", wantErr: true},
		{key: ".", wantErr: true},
	}

	for _, tt := range tests {
		got, err := SanitizeObjectKey(tt.key)
		if (err != nil) != tt.wantErr {
			t.Errorf("SanitizeObjectKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			continue
		}

		if err == nil && got != tt.key {
			t.Errorf("SanitizeObjectKey(%q) = %q, want unchanged", tt.key, got)
		}
	}
}

func TestBuildReceivers(t *testing.T) {
	t.Parallel()

	pemA, pubA := testReceiverPublicKeyPEM(t)
	pemB, pubB := testReceiverPublicKeyPEM(t)

	receivers, err := buildReceivers([]FileReceiver{
		{ID: "a", PublicKey: pemA, Path: "/mnt/a"},
		{ID: "b", PublicKey: pemB, Path: "/mnt/b", Retention: "7d"},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := map[string]ResolvedReceiver{
		"a": {ID: "a", PublicKey: pubA, Path: "/mnt/a"},
		"b": {ID: "b", PublicKey: pubB, Path: "/mnt/b", Retention: 7 * 24 * time.Hour},
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

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{{PublicKey: pemText, Path: "/mnt/a"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing id, got nil")
	}
}

func TestBuildReceiversRequiresPath(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: pemText}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing path, got nil")
	}
}

func TestBuildReceiversRequiresPublicKey(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{{ID: "a", Path: "/mnt/a"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing public-key, got nil")
	}
}

func TestBuildReceiversRejectsInvalidPublicKey(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: "not a PEM key", Path: "/mnt/a"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for invalid public-key, got nil")
	}
}

func TestBuildReceiversRejectsNonRSAPublicKey(t *testing.T) {
	t.Parallel()

	// A PEM block of the right type but the wrong key algorithm (an EC key,
	// rather than RSA) must also be rejected, since signRemoteAuthToken/
	// verifyRemoteAuthToken only support RS256.
	const ecPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEF4MRFTT9HV60ttWqYkFekzGqdpVY
If1SoUSBRfFVHGlXZJjmfRQxikr35aLMtrCtQ4GvhyLhd81I0HfA3+H0gg==
-----END PUBLIC KEY-----`

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: ecPublicKeyPEM, Path: "/mnt/a"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for a non-RSA public-key, got nil")
	}
}

func TestListReceiverFilesMissingRoot(t *testing.T) {
	t.Parallel()

	recv := ResolvedReceiver{ID: "a", Path: filepath.Join(t.TempDir(), "does-not-exist")}

	files, err := ListReceiverFiles(recv)
	if err != nil {
		t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("ListReceiverFiles() = %+v, want empty", files)
	}
}

func TestListReceiverFilesListsNestedObjectsSortedByCreatedTimeAscending(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	newerPath := filepath.Join(root, "b.gpg")
	olderPath := filepath.Join(root, "subdir", "a.gpg")

	writeFile(t, newerPath, "bbb")
	writeFile(t, olderPath, "a")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	older := time.Now().Add(-time.Hour)
	newer := time.Now()

	if err := os.Chtimes(olderPath, older, older); err != nil {
		t.Fatalf("os.Chtimes(%q) unexpected error: %v", olderPath, err)
	}

	if err := os.Chtimes(newerPath, newer, newer); err != nil {
		t.Fatalf("os.Chtimes(%q) unexpected error: %v", newerPath, err)
	}

	recv := ResolvedReceiver{ID: "a", Path: root}

	files, err := ListReceiverFiles(recv)
	if err != nil {
		t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("ListReceiverFiles() = %+v, want 2 entries", files)
	}

	if files[0].Key != "subdir/a.gpg" || files[0].Size != 1 {
		t.Errorf("files[0] = %+v, want key subdir/a.gpg size 1 (oldest)", files[0])
	}

	if files[1].Key != "b.gpg" || files[1].Size != 3 {
		t.Errorf("files[1] = %+v, want key b.gpg size 3 (newest)", files[1])
	}
}

// writeFile creates path (and any missing parent directories) with contents.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating directory for %q: %v", path, err)
	}

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %q: %v", path, err)
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

func TestBuildReceiversStaleAfterAndWebhook(t *testing.T) {
	t.Parallel()

	pemText, pub := testReceiverPublicKeyPEM(t)

	receivers, err := buildReceivers([]FileReceiver{
		{ID: "a", PublicKey: pemText, Path: "/mnt/a", StaleAfter: "6h", Webhook: fileWebhook{URL: "https://example.com/hook"}},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := ResolvedReceiver{
		ID: "a", PublicKey: pub, Path: "/mnt/a",
		StaleAfter: 6 * time.Hour,
		Webhook:    ResolvedWebhook{URL: "https://example.com/hook", Method: http.MethodPost},
	}

	if got := receivers["a"]; !reflect.DeepEqual(got, want) {
		t.Errorf("buildReceivers()[%q] = %+v, want %+v", "a", got, want)
	}
}

func TestBuildReceiversStaleAfterRequiresWebhook(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: pemText, Path: "/mnt/a", StaleAfter: "6h"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for stale-after without webhook.url, got nil")
	}
}

func TestBuildReceiversWebhookRequiresStaleAfter(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: pemText, Path: "/mnt/a", Webhook: fileWebhook{URL: "https://example.com/hook"}}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.url without stale-after, got nil")
	}
}

func TestBuildReceiversWebhookMethodHeadersAndBody(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	receivers, err := buildReceivers([]FileReceiver{
		{
			ID: "a", PublicKey: pemText, Path: "/mnt/a", StaleAfter: "6h",
			Webhook: fileWebhook{
				URL:     "https://example.com/hook",
				Method:  "put",
				Headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Authorization": "Bearer tok"},
				Body:    `{"text":"{receiver_id} stale since {last_received}"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := ResolvedWebhook{
		URL:    "https://example.com/hook",
		Method: http.MethodPut, // lowercase "put" in the config file is normalized to uppercase
		Headers: map[string]string{
			"Content-Type":  "application/json; charset=utf-8",
			"Authorization": "Bearer tok",
		},
		Body: `{"text":"{receiver_id} stale since {last_received}"}`,
	}

	if got := receivers["a"].Webhook; !reflect.DeepEqual(got, want) {
		t.Errorf("buildReceivers()[%q].Webhook = %+v, want %+v", "a", got, want)
	}
}

func TestBuildReceiversWebhookMethodDefaultsToPost(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	receivers, err := buildReceivers([]FileReceiver{
		{ID: "a", PublicKey: pemText, Path: "/mnt/a", StaleAfter: "6h", Webhook: fileWebhook{URL: "https://example.com/hook"}},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	if got := receivers["a"].Webhook.Method; got != http.MethodPost {
		t.Errorf("webhook.method = %q, want %q when unset", got, http.MethodPost)
	}
}

func TestBuildReceiversWebhookBodyRequiresURL(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: pemText, Path: "/mnt/a", Webhook: fileWebhook{Body: "custom"}}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.body without webhook.url, got nil")
	}
}

func TestBuildReceiversWebhookHeadersRequireURL(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{
		{ID: "a", PublicKey: pemText, Path: "/mnt/a", Webhook: fileWebhook{Headers: map[string]string{"X-Test": "y"}}},
	})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.headers without webhook.url, got nil")
	}
}

func TestBuildReceiversWebhookMethodRequiresURL(t *testing.T) {
	t.Parallel()

	pemText, _ := testReceiverPublicKeyPEM(t)

	_, err := buildReceivers([]FileReceiver{{ID: "a", PublicKey: pemText, Path: "/mnt/a", Webhook: fileWebhook{Method: "PUT"}}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for webhook.method without webhook.url, got nil")
	}
}

func TestLastReceivedAtMissingRoot(t *testing.T) {
	t.Parallel()

	recv := ResolvedReceiver{ID: "a", Path: filepath.Join(t.TempDir(), "does-not-exist")}

	_, ok, err := LastReceivedAt(recv)
	if err != nil {
		t.Fatalf("LastReceivedAt() unexpected error: %v", err)
	}

	if ok {
		t.Error("LastReceivedAt() ok = true, want false for a missing root")
	}
}

func TestLastReceivedAtReturnsNewestModTime(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	older := filepath.Join(root, "older.gpg")
	newer := filepath.Join(root, "subdir", "newer.gpg")

	writeFile(t, older, "a")
	writeFile(t, newer, "b")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	olderTime := time.Now().Add(-2 * time.Hour)
	newerTime := time.Now().Add(-1 * time.Hour)

	if err := os.Chtimes(older, olderTime, olderTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", older, err)
	}

	if err := os.Chtimes(newer, newerTime, newerTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", newer, err)
	}

	// A hidden temp file, if it were counted, would appear newest of all
	// (its default mtime is "now"), so this also verifies it's skipped.
	got, ok, err := LastReceivedAt(ResolvedReceiver{ID: "a", Path: root})
	if err != nil {
		t.Fatalf("LastReceivedAt() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("LastReceivedAt() ok = false, want true")
	}

	if !got.Equal(newerTime) {
		t.Errorf("LastReceivedAt() = %v, want %v", got, newerTime)
	}
}

func TestReceiverStatusStoreSeedLastEventSuccess(t *testing.T) {
	t.Parallel()

	receivers := map[string]ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := NewReceiverStatusStore(receivers)

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SeedLastEvent("a", ReceiverEvent{At: at, ReceiverID: "a", Kind: ReceiverEventReceive, Key: "obj.gpg", Success: true})

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %+v, want 1 entry", snap)
	}

	if snap[0].State != StateOK || snap[0].LastKey != "obj.gpg" || !snap[0].LastSeen.Equal(at) || snap[0].Error != "" {
		t.Errorf("snapshot()[0] = %+v, want state ok, last_key obj.gpg, last_seen %v, no error", snap[0], at)
	}
}

func TestReceiverStatusStoreSeedLastEventFailure(t *testing.T) {
	t.Parallel()

	receivers := map[string]ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := NewReceiverStatusStore(receivers)

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SeedLastEvent("a", ReceiverEvent{At: at, ReceiverID: "a", Kind: ReceiverEventDelete, Key: "obj.gpg", Success: false, Error: "disk full"})

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %+v, want 1 entry", snap)
	}

	if snap[0].State != StateFailed || snap[0].Error != "disk full" {
		t.Errorf("snapshot()[0] = %+v, want state failed with error %q", snap[0], "disk full")
	}
}

func TestReceiverStatusStoreSeedLastEventUnknownReceiverIsNoop(t *testing.T) {
	t.Parallel()

	store := NewReceiverStatusStore(map[string]ResolvedReceiver{})

	store.SeedLastEvent("does-not-exist", ReceiverEvent{Success: true})

	if snap := store.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot() = %+v, want none", snap)
	}
}
