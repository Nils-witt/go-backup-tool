package pipeline

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup"
)

func TestRemoteObjectURL(t *testing.T) {
	t.Parallel()

	tgt := &backup.Target{Endpoint: "https://backup2.example.com:8443/", Bucket: "from primary"}

	got := RemoteObjectURL(tgt, "backup 20260101.gpg")
	want := "https://backup2.example.com:8443/api/v1/objects/from%20primary/backup%2020260101.gpg"

	if got != want {
		t.Errorf("RemoteObjectURL() = %q, want %q", got, want)
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
	cfg := &backup.Config{Key: "backup.gpg", Identity: id}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

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

	if err := backup.VerifyRemoteAuthToken(strings.TrimPrefix(gotAuth, prefix), &key.PublicKey, "instance-a"); err != nil {
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

	cfg := &backup.Config{Key: "backup.gpg"} // identity left nil: loadServerIdentity failed at startup
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

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

	cfg := &backup.Config{Key: "backup.gpg", Identity: testServerIdentity(t)}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	err := UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("UploadToRemote() error = %v, want it to mention 401", err)
	}
}

func TestDeleteRemoteObject(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := &backup.Config{Key: "backup.gpg", Identity: testServerIdentity(t)}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := DeleteRemoteObject(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("DeleteRemoteObject() unexpected error: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}

	if gotPath != "/api/v1/objects/instance-a/backup.gpg" {
		t.Errorf("path = %q, want /api/v1/objects/instance-a/backup.gpg", gotPath)
	}
}

func TestDeleteRemoteObjectNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &backup.Config{Key: "backup.gpg", Identity: testServerIdentity(t)}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "no-such-id"}

	err := DeleteRemoteObject(t.Context(), cfg, tgt)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("DeleteRemoteObject() error = %v, want it to mention 404", err)
	}
}
