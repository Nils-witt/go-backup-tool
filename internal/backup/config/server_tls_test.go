package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// selfSignedPEM returns a fresh self-signed CA certificate, PEM-encoded.
func selfSignedPEM(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
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

func TestTargetTLS_ClientConfig(t *testing.T) {
	t.Parallel()

	caPEM := selfSignedPEM(t)

	t.Run("zero value is the default", func(t *testing.T) {
		t.Parallel()

		cfg, err := TargetTLS{}.ClientConfig()
		if err != nil || cfg != nil {
			t.Fatalf("ClientConfig() = %v, %v, want nil, nil", cfg, err)
		}
	})

	t.Run("insecure", func(t *testing.T) {
		t.Parallel()

		cfg, err := TargetTLS{InsecureSkipVerify: true}.ClientConfig()
		if err != nil {
			t.Fatalf("ClientConfig() unexpected error: %v", err)
		}

		if !cfg.InsecureSkipVerify || cfg.VerifyConnection != nil {
			t.Errorf("InsecureSkipVerify = %v, VerifyConnection set = %v, want true, false", cfg.InsecureSkipVerify, cfg.VerifyConnection != nil)
		}
	})

	t.Run("custom ca", func(t *testing.T) {
		t.Parallel()

		cfg, err := TargetTLS{CA: caPEM}.ClientConfig()
		if err != nil {
			t.Fatalf("ClientConfig() unexpected error: %v", err)
		}

		if cfg.RootCAs == nil || cfg.InsecureSkipVerify {
			t.Errorf("RootCAs set = %v, InsecureSkipVerify = %v, want true, false", cfg.RootCAs != nil, cfg.InsecureSkipVerify)
		}
	})

	t.Run("skip hostname verifies the chain itself", func(t *testing.T) {
		t.Parallel()

		cfg, err := TargetTLS{CA: caPEM, SkipHostnameVerify: true}.ClientConfig()
		if err != nil {
			t.Fatalf("ClientConfig() unexpected error: %v", err)
		}

		if !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil {
			t.Errorf("InsecureSkipVerify = %v, VerifyConnection set = %v, want true, true", cfg.InsecureSkipVerify, cfg.VerifyConnection != nil)
		}
	})

	t.Run("invalid ca", func(t *testing.T) {
		t.Parallel()

		if _, err := (TargetTLS{CA: "not a certificate"}).ClientConfig(); err == nil {
			t.Error("ClientConfig() with an invalid CA: want an error")
		}
	})
}

func TestResolveServerTLS(t *testing.T) {
	t.Parallel()

	caPEM := selfSignedPEM(t)

	tests := []struct {
		name    string
		server  FileServer
		wantErr string
		want    TargetTLS
	}{
		{
			name:   "remote without tls settings",
			server: FileServer{Name: "r", Type: "remote", Endpoint: "http://backup.example.com"},
		},
		{
			name:   "remote insecure",
			server: FileServer{Name: "r", Type: "remote", Endpoint: "https://backup.example.com", TLSInsecureSkipVerify: true},
			want:   TargetTLS{InsecureSkipVerify: true},
		},
		{
			name: "remote custom ca without hostname check",
			server: FileServer{
				Name: "r", Type: "remote", Endpoint: "HTTPS://10.0.0.5:8443",
				TLSCA: "\n" + caPEM + "\n", TLSSkipHostnameVerify: true,
			},
			want: TargetTLS{CA: strings.TrimSpace(caPEM), SkipHostnameVerify: true},
		},
		{
			name:    "local with tls settings",
			server:  FileServer{Name: "l", Type: "local", Path: "/backups", TLSInsecureSkipVerify: true},
			wantErr: "not valid for type: local",
		},
		{
			name:    "http endpoint with tls settings",
			server:  FileServer{Name: "r", Type: "remote", Endpoint: "http://backup.example.com", TLSCA: caPEM},
			wantErr: "need an https endpoint",
		},
		{
			name: "insecure with custom ca",
			server: FileServer{
				Name: "r", Type: "remote", Endpoint: "https://backup.example.com",
				TLSInsecureSkipVerify: true, TLSCA: caPEM,
			},
			wantErr: "can't be combined",
		},
		{
			name: "insecure without hostname check",
			server: FileServer{
				Name: "r", Type: "remote", Endpoint: "https://backup.example.com",
				TLSInsecureSkipVerify: true, TLSSkipHostnameVerify: true,
			},
			wantErr: "can't be combined",
		},
		{
			name:    "invalid ca",
			server:  FileServer{Name: "r", Type: "remote", Endpoint: "https://backup.example.com", TLSCA: "garbage"},
			wantErr: "no valid PEM certificate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveServer(tt.server)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveServer() error = %v, want it to contain %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("ResolveServer() unexpected error: %v", err)
			}

			if got.tls != tt.want {
				t.Errorf("ResolveServer().tls = %+v, want %+v", got.tls, tt.want)
			}
		})
	}
}
