package receiver

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
)

// newReceiverMux builds the same mux backup.StartWebUI registers the
// receiver routes on, without a real listener, so an interop test can
// exercise the client (UploadToRemote/DeleteRemoteObject) against the real
// server-side handlers rather than a hand-rolled stand-in.
func newReceiverMux(receivers map[string]backup.ResolvedReceiver) *http.ServeMux {
	status := backup.NewReceiverStatusStore(receivers)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", HandleReceiveObject(receivers, status, discardLogger, nil))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", HandleDeleteObject(receivers, status, discardLogger, nil))

	return mux
}

func TestRemoteTargetInteropWithReceiver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id, key := testServerIdentityAndKey(t)
	receivers := map[string]backup.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &backup.Config{Key: "backup-20260101.gpg", Identity: id}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	const content = "encrypted backup bytes"

	if err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("pipeline.UploadToRemote() unexpected error: %v", err)
	}

	got, err := os.ReadFile(dir + "/backup-20260101.gpg") //nolint:gosec // dir is t.TempDir() plus a fixed test literal
	if err != nil {
		t.Fatalf("reading object the receiver wrote: %v", err)
	}

	if string(got) != content {
		t.Errorf("received content = %q, want %q", got, content)
	}

	if err := pipeline.DeleteRemoteObject(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("pipeline.DeleteRemoteObject() unexpected error: %v", err)
	}

	if _, err := os.Stat(dir + "/backup-20260101.gpg"); err == nil {
		t.Error("object still present on the receiver after pipeline.DeleteRemoteObject()")
	}
}

func TestRemoteTargetInteropWrongIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, key := testServerIdentityAndKey(t)
	receivers := map[string]backup.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	// cfg signs with a different identity than the one the receiver trusts,
	// so its signature won't verify against the receiver's configured
	// public-key:.
	cfg := &backup.Config{Key: "backup.gpg", Identity: testServerIdentity(t)}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("pipeline.UploadToRemote() with the wrong identity error = %v, want it to mention 401", err)
	}
}

func TestRemoteTargetInteropNoAuthorizationHeader(t *testing.T) {
	t.Parallel()

	_, key := testServerIdentityAndKey(t)
	receivers := map[string]backup.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: t.TempDir()},
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

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]backup.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &backup.Config{Key: "backup.gpg", Identity: id}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "no-such-id"}

	err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("pipeline.UploadToRemote() with unknown id error = %v, want it to mention 404", err)
	}
}

// newReceiverMuxWithDB is newReceiverMux, but wiring db through to
// HandleReceiveObject/HandleDeleteObject instead of nil, so a
// test can inspect what they recorded to it (retention tracking,
// receiver_events).
func newReceiverMuxWithDB(receivers map[string]backup.ResolvedReceiver, db *sql.DB) *http.ServeMux {
	status := backup.NewReceiverStatusStore(receivers)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", HandleReceiveObject(receivers, status, discardLogger, db))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", HandleDeleteObject(receivers, status, discardLogger, db))

	return mux
}

func TestHandleReceiveAndDeleteObjectRecordReceiverEvents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	db, err := backup.OpenScheduleStateDB(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]backup.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: filepath.Join(dir, "objects")},
	}

	srv := httptest.NewServer(newReceiverMuxWithDB(receivers, db))
	defer srv.Close()

	cfg := &backup.Config{Key: "backup.gpg", Identity: id}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	const content = "ciphertext bytes"

	if err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("pipeline.UploadToRemote() error: %v", err)
	}

	if err := pipeline.DeleteRemoteObject(t.Context(), cfg, tgt); err != nil {
		t.Fatalf("pipeline.DeleteRemoteObject() error: %v", err)
	}

	summaries, err := backup.SummarizeReceiverEvents(t.Context(), db, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SummarizeReceiverEvents() error: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("SummarizeReceiverEvents() = %+v, want exactly one receiver's summary", summaries)
	}

	if got := summaries[0]; got.ReceiverID != "instance-a" || got.FilesReceived != 1 || got.BytesReceived != int64(len(content)) || got.Errors != 0 {
		t.Errorf("summary = %+v, want {instance-a, 1 file, %d bytes, 0 errors}", got, len(content))
	}
}

func TestHandleReceiveObjectRecordsFailedReceiverEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	db, err := backup.OpenScheduleStateDB(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenScheduleStateDB() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	// A plain file where the receiver's root directory should be: writing
	// "backup.gpg" under it needs os.MkdirAll(root) to succeed, and MkdirAll
	// fails when root already exists as a non-directory.
	root := filepath.Join(dir, "not-a-directory")
	writeFile(t, root, "occupied")

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]backup.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: root},
	}

	srv := httptest.NewServer(newReceiverMuxWithDB(receivers, db))
	defer srv.Close()

	cfg := &backup.Config{Key: "backup.gpg", Identity: id}
	tgt := &backup.Target{Kind: backup.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("pipeline.UploadToRemote() error = nil, want a failure since the receiver's root isn't a directory")
	}

	errs, err := backup.ReadReceiverErrorEvents(t.Context(), db, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReadReceiverErrorEvents() error: %v", err)
	}

	if len(errs) != 1 || errs[0].ReceiverID != "instance-a" || errs[0].Kind != backup.ReceiverEventReceive {
		t.Fatalf("ReadReceiverErrorEvents() = %+v, want one failed receive for instance-a", errs)
	}
}
