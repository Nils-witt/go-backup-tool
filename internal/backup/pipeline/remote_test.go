package pipeline

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteAuth"
)

func TestRemoteObjectURL(t *testing.T) {
	t.Parallel()

	tgt := &config.Target{Endpoint: "https://backup2.example.com:8443/", Bucket: "from primary"}

	tests := []struct {
		name     string
		key      string
		file     string
		expected string
	}{
		{
			name:     "flat object",
			key:      "backup 20260101.gpg",
			expected: "https://backup2.example.com:8443/api/v1/objects/from%20primary/backup%2020260101.gpg",
		},
		{
			name:     "file of a multi-file backup",
			key:      "backup 20260101",
			file:     "db.gpg",
			expected: "https://backup2.example.com:8443/api/v1/backups/from%20primary/backup%2020260101",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := RemoteObjectURL(tgt, tt.key, tt.file); got != tt.expected {
				t.Errorf("RemoteObjectURL() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestUploadToRemote(t *testing.T) {
	t.Parallel()

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	id, key := testServerIdentityAndKey(t)
	cfg := &config.Config{Key: "backup.gpg", Identity: id}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("UploadToRemote() unexpected error: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}

	if gotPath != "/api/v1/objects/instance-a/backup.gpg" {
		t.Errorf("path = %q, want /api/v1/objects/instance-a/backup.gpg", gotPath)
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(gotAuth, prefix) {
		t.Fatalf("Authorization = %q, want it to start with %q", gotAuth, prefix)
	}

	if _, err := remoteAuth.VerifyRemoteAuthToken(strings.TrimPrefix(gotAuth, prefix), &key.PublicKey, "instance-a", ""); err != nil {
		t.Errorf("token sent as Authorization did not verify: %v", err)
	}

	if gotBody != "ciphertext" {
		t.Errorf("body = %q, want %q", gotBody, "ciphertext")
	}
}

func TestUploadToRemoteNoIdentity(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg"} // identity left nil: loadServerIdentity failed at startup
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	err := UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("UploadToRemote() with no identity error = %v, want it to mention identity", err)
	}
}

func TestUploadToRemoteUnauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg", Identity: testServerIdentity(t)}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	err := UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("UploadToRemote() error = %v, want it to mention 401", err)
	}
}

func TestUploadToRemoteFileSendsContentLength(t *testing.T) {
	t.Parallel()

	var (
		gotLength   int64
		gotEncoding []string
		gotBody     string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLength = r.ContentLength
		gotEncoding = r.TransferEncoding

		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "staged")
	if err := os.WriteFile(path, []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path) //nolint:gosec // path is this test's own t.TempDir() file, not untrusted input
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	id, _ := testServerIdentityAndKey(t)
	cfg := &config.Config{Key: "backup.gpg", Identity: id}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := UploadToRemote(t.Context(), cfg, tgt, f); err != nil {
		t.Fatalf("UploadToRemote() unexpected error: %v", err)
	}

	if gotLength != int64(len("ciphertext")) || len(gotEncoding) != 0 {
		t.Errorf("ContentLength = %d, TransferEncoding = %v; want %d and none", gotLength, gotEncoding, len("ciphertext"))
	}

	if gotBody != "ciphertext" {
		t.Errorf("body = %q, want %q", gotBody, "ciphertext")
	}
}

// otherCAPEM returns a fresh self-signed CA certificate, PEM-encoded, that
// signed nothing an httptest server presents.
func otherCAPEM(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestUploadToRemoteTLS(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close) // the subtests are parallel: they run after this function returns

	// httptest's self-signed certificate names 127.0.0.1 and example.com,
	// not localhost: reaching it as localhost is a hostname mismatch.
	serverCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	byIP := srv.URL
	byName := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	tests := []struct {
		name     string
		endpoint string
		tls      config.TargetTLS
		wantErr  string
	}{
		{name: "default rejects an unknown authority", endpoint: byIP, wantErr: "certificate"},
		{name: "insecure accepts anything", endpoint: byName, tls: config.TargetTLS{InsecureSkipVerify: true}},
		{name: "custom ca", endpoint: byIP, tls: config.TargetTLS{CA: serverCA}},
		{name: "custom ca still checks the hostname", endpoint: byName, tls: config.TargetTLS{CA: serverCA}, wantErr: "localhost"},
		{name: "custom ca without hostname check", endpoint: byName, tls: config.TargetTLS{CA: serverCA, SkipHostnameVerify: true}},
		{
			name: "no hostname check still verifies the chain", endpoint: byName,
			tls: config.TargetTLS{CA: otherCAPEM(t), SkipHostnameVerify: true}, wantErr: "unknown authority",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Key: "backup.gpg", Identity: testServerIdentity(t)}
			tgt := &config.Target{ServerName: "offsite", Kind: config.ServerKindRemote, Endpoint: tt.endpoint, Bucket: "instance-a", TLS: tt.tls}

			err := UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("ciphertext"))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("UploadToRemote() unexpected error: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("UploadToRemote() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRemoteHTTPClientReusesClients(t *testing.T) {
	t.Parallel()

	insecure := config.TargetTLS{InsecureSkipVerify: true}

	def, err := remoteHTTPClient(&config.Target{})
	if err != nil || def != remoteDefaultClient {
		t.Fatalf("remoteHTTPClient(default) = %p, %v, want remoteDefaultClient", def, err)
	}

	a, errA := remoteHTTPClient(&config.Target{ServerName: "a", TLS: insecure})
	b, errB := remoteHTTPClient(&config.Target{ServerName: "b", TLS: insecure})

	if errA != nil || errB != nil {
		t.Fatalf("remoteHTTPClient() errors: %v, %v", errA, errB)
	}

	if a != b || a == remoteDefaultClient {
		t.Errorf("same TLS settings got clients %p and %p, want one shared non-default client", a, b)
	}
}
