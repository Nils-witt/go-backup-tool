package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
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
