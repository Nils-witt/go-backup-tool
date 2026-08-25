package backup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRemoteObjectURL(t *testing.T) {
	t.Parallel()

	tgt := &target{endpoint: "https://backup2.example.com:8443/", bucket: "from primary"}

	got := remoteObjectURL(tgt, "backup 20260101.gpg")
	want := "https://backup2.example.com:8443/api/v1/objects/from%20primary/backup%2020260101.gpg"

	if got != want {
		t.Errorf("remoteObjectURL() = %q, want %q", got, want)
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

	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a", token: "shh-token"}

	if err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("uploadToRemote() unexpected error: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}

	if gotPath != "/api/v1/objects/instance-a/backup.gpg" {
		t.Errorf("path = %q, want /api/v1/objects/instance-a/backup.gpg", gotPath)
	}

	if gotAuth != "Bearer shh-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer shh-token")
	}

	if gotBody != "ciphertext" {
		t.Errorf("body = %q, want %q", gotBody, "ciphertext")
	}
}

func TestUploadToRemoteUnauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a", token: "wrong"}

	err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("uploadToRemote() error = %v, want it to mention 401", err)
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

	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a", token: "shh-token"}

	if err := deleteRemoteObject(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("deleteRemoteObject() unexpected error: %v", err)
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

	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "no-such-id", token: "shh-token"}

	err := deleteRemoteObject(t.Context(), cfg, tgt)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("deleteRemoteObject() error = %v, want it to mention 404", err)
	}
}

// newReceiverMux builds the same mux startWebUI registers the receiver
// routes on, without a real listener, so an interop test can exercise the
// client (uploadToRemote/deleteRemoteObject) against the real server-side
// handlers rather than a hand-rolled stand-in.
func newReceiverMux(receivers map[string]resolvedReceiver) *http.ServeMux {
	status := newReceiverStatusStore(receivers)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", handleReceiveObject(receivers, status, discardLogger))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", handleDeleteObject(receivers, status, discardLogger))

	return mux
}

func TestRemoteTargetInteropWithReceiver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", token: "shared-secret", path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config{key: "backup-20260101.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a", token: "shared-secret"}

	const content = "encrypted backup bytes"

	if err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("uploadToRemote() unexpected error: %v", err)
	}

	got, err := os.ReadFile(dir + "/backup-20260101.gpg") //nolint:gosec // dir is t.TempDir() plus a fixed test literal
	if err != nil {
		t.Fatalf("reading object the receiver wrote: %v", err)
	}

	if string(got) != content {
		t.Errorf("received content = %q, want %q", got, content)
	}

	if err := deleteRemoteObject(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("deleteRemoteObject() unexpected error: %v", err)
	}

	if _, err := os.Stat(dir + "/backup-20260101.gpg"); err == nil {
		t.Error("object still present on the receiver after deleteRemoteObject()")
	}
}

func TestRemoteTargetInteropWrongToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", token: "shared-secret", path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a", token: "wrong-token"}

	err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("uploadToRemote() with wrong token error = %v, want it to mention 401", err)
	}
}

func TestRemoteTargetInteropUnknownID(t *testing.T) {
	t.Parallel()

	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", token: "shared-secret", path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config{key: "backup.gpg"}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "no-such-id", token: "shared-secret"}

	err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("uploadToRemote() with unknown id error = %v, want it to mention 404", err)
	}
}
