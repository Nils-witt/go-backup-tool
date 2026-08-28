package backup

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	id := testServerIdentity(t)
	cfg := &config{key: "backup.gpg", identity: id}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

	if err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("ciphertext")); err != nil {
		t.Fatalf("uploadToRemote() unexpected error: %v", err)
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

	if err := verifyRemoteAuthToken(strings.TrimPrefix(gotAuth, prefix), &id.privateKey.PublicKey, "instance-a"); err != nil {
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

	cfg := &config{key: "backup.gpg"} // identity left nil: loadServerIdentity failed at startup
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

	err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("uploadToRemote() with no identity error = %v, want it to mention identity", err)
	}
}

func TestUploadToRemoteUnauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &config{key: "backup.gpg", identity: testServerIdentity(t)}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

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

	cfg := &config{key: "backup.gpg", identity: testServerIdentity(t)}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

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

	cfg := &config{key: "backup.gpg", identity: testServerIdentity(t)}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "no-such-id"}

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
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", handleReceiveObject(receivers, status, discardLogger, nil))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", handleDeleteObject(receivers, status, discardLogger, nil))

	return mux
}

func TestRemoteTargetInteropWithReceiver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id := testServerIdentity(t)
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", publicKey: &id.privateKey.PublicKey, path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config{key: "backup-20260101.gpg", identity: id}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

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

func TestRemoteTargetInteropWrongIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", publicKey: &testServerIdentity(t).privateKey.PublicKey, path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	// cfg signs with a different identity than the one the receiver trusts,
	// so its signature won't verify against the receiver's configured
	// public-key:.
	cfg := &config{key: "backup.gpg", identity: testServerIdentity(t)}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

	err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("uploadToRemote() with the wrong identity error = %v, want it to mention 401", err)
	}
}

func TestRemoteTargetInteropNoAuthorizationHeader(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", publicKey: &id.privateKey.PublicKey, path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/api/v1/objects/instance-a/backup.gpg", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (no Authorization header at all)", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestRemoteTargetInteropUnknownID(t *testing.T) {
	t.Parallel()

	id := testServerIdentity(t)
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", publicKey: &id.privateKey.PublicKey, path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config{key: "backup.gpg", identity: id}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "no-such-id"}

	err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("uploadToRemote() with unknown id error = %v, want it to mention 404", err)
	}
}

// newReceiverMuxWithDB is newReceiverMux, but wiring db through to
// handleReceiveObject/handleDeleteObject instead of nil, so a test can
// inspect what they recorded to it (retention tracking, receiver_events).
func newReceiverMuxWithDB(receivers map[string]resolvedReceiver, db *sql.DB) *http.ServeMux {
	status := newReceiverStatusStore(receivers)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", handleReceiveObject(receivers, status, discardLogger, db))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", handleDeleteObject(receivers, status, discardLogger, db))

	return mux
}

func TestHandleReceiveAndDeleteObjectRecordReceiverEvents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	db, err := openScheduleStateDB(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("openScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	id := testServerIdentity(t)
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", publicKey: &id.privateKey.PublicKey, path: filepath.Join(dir, "objects")},
	}

	srv := httptest.NewServer(newReceiverMuxWithDB(receivers, db))
	defer srv.Close()

	cfg := &config{key: "backup.gpg", identity: id}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

	const content = "ciphertext bytes"

	if err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("uploadToRemote() error: %v", err)
	}

	if err := deleteRemoteObject(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("deleteRemoteObject() error: %v", err)
	}

	summaries, err := summarizeReceiverEvents(t.Context(), db, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("summarizeReceiverEvents() error: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("summarizeReceiverEvents() = %+v, want exactly one receiver's summary", summaries)
	}

	if got := summaries[0]; got.ReceiverID != "instance-a" || got.FilesReceived != 1 || got.BytesReceived != int64(len(content)) || got.Errors != 0 {
		t.Errorf("summary = %+v, want {instance-a, 1 file, %d bytes, 0 errors}", got, len(content))
	}
}

func TestHandleReceiveObjectRecordsFailedReceiverEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	db, err := openScheduleStateDB(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("openScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	// A plain file where the receiver's root directory should be: writing
	// "backup.gpg" under it needs os.MkdirAll(root) to succeed, and MkdirAll
	// fails when root already exists as a non-directory.
	root := filepath.Join(dir, "not-a-directory")
	writeFile(t, root, "occupied")

	id := testServerIdentity(t)
	receivers := map[string]resolvedReceiver{
		"instance-a": {id: "instance-a", publicKey: &id.privateKey.PublicKey, path: root},
	}

	srv := httptest.NewServer(newReceiverMuxWithDB(receivers, db))
	defer srv.Close()

	cfg := &config{key: "backup.gpg", identity: id}
	tgt := &target{kind: serverKindRemote, endpoint: srv.URL, bucket: "instance-a"}

	if err := uploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("uploadToRemote() error = nil, want a failure since the receiver's root isn't a directory")
	}

	errs, err := readReceiverErrorEvents(t.Context(), db, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("readReceiverErrorEvents() error: %v", err)
	}

	if len(errs) != 1 || errs[0].ReceiverID != "instance-a" || errs[0].Kind != receiverEventReceive {
		t.Fatalf("readReceiverErrorEvents() = %+v, want one failed receive for instance-a", errs)
	}
}
