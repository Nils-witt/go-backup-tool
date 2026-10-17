package receiver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// newReceiverMux builds the same mux backup.StartWebUI registers the
// receiver routes on, without a real listener, so an interop test can
// exercise the client (UploadToRemote) against the real
// server-side handlers rather than a hand-rolled stand-in.
func newReceiverMux(receivers map[string]config.ResolvedReceiver) *http.ServeMux {
	receiverStatus := status.NewReceiverStore(receivers)

	mux := http.NewServeMux()
	RegisterRoutes(mux, backup.NewReceiverRegistry(receivers), receiverStatus, discardLogger, nil)

	return mux
}

func TestRemoteTargetInteropMultiFileBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id, key := testServerIdentityAndKey(t)
	recv := config.ResolvedReceiver{ID: "instance-a", PublicKey: &key.PublicKey, Path: dir}

	srv := httptest.NewServer(newReceiverMux(map[string]config.ResolvedReceiver{recv.ID: recv}))
	defer srv.Close()

	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: recv.ID}

	files := map[string]string{"db.gpg": "db bytes", "uploads.gpg": "upload bytes"}
	for name, content := range files {
		cfg := &config.Config{Key: "backup-20260101", File: name}
		if err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader(content)); err != nil {
			t.Fatalf("pipeline.UploadToRemote(%s) unexpected error: %v", name, err)
		}
	}

	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dir, "backup-20260101", name)) //nolint:gosec // dir is t.TempDir() plus fixed test literals
		if err != nil || string(got) != content {
			t.Errorf("%s = %q, %v; want %q", name, got, err, content)
		}
	}

	listed, err := backup.ListReceiverFiles(recv)
	if err != nil {
		t.Fatalf("ListReceiverFiles() error: %v", err)
	}

	if len(listed) != 2 {
		t.Fatalf("ListReceiverFiles() = %+v, want the backup's two files", listed)
	}

	for _, f := range listed {
		if f.Key != "backup-20260101" || files[f.File] == "" {
			t.Errorf("listed file = %+v, want key backup-20260101 and one of the backup's files", f)
		}
	}
}

// putBackupFile sends a PUT to the multi-file route for key, with rawQuery
// attached as-is and a token signed with id for bound.
func putBackupFile(t *testing.T, baseURL string, id *identity.ServerIdentity, recvID, key, rawQuery string, bound remoteauth.Request) *http.Response {
	t.Helper()

	token, err := id.SignRequest(recvID, bound)
	if err != nil {
		t.Fatalf("signing request: %v", err)
	}

	u := baseURL + "/api/v1/backups/" + recvID + "/" + key + "?" + rawQuery

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, u, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}

	return resp
}

func TestHandleReceiveBackupFileRejects(t *testing.T) {
	t.Parallel()

	const digestOfX = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"

	boundFor := func(file string) remoteauth.Request {
		return remoteauth.Request{Method: http.MethodPut, Key: "k", File: file, BodySHA256: digestOfX}
	}

	tests := []struct {
		name     string
		rawQuery string
		bound    remoteauth.Request
		expected int
	}{
		{name: "missing file", rawQuery: "", bound: boundFor(""), expected: http.StatusBadRequest},
		{name: "hidden file", rawQuery: "file=.gbt-backup", bound: boundFor(".gbt-backup"), expected: http.StatusBadRequest},
		{name: "file with a slash", rawQuery: "file=a%2Fb", bound: boundFor("a/b"), expected: http.StatusBadRequest},
		{name: "token for another file", rawQuery: "file=a.gpg", bound: boundFor("b.gpg"), expected: http.StatusUnauthorized},
		{name: "unbound token", rawQuery: "file=a.gpg", bound: remoteauth.Request{}, expected: http.StatusUnauthorized},
		{name: "valid", rawQuery: "file=a.gpg", bound: boundFor("a.gpg"), expected: http.StatusCreated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			id, key := testServerIdentityAndKey(t)
			receivers := map[string]config.ResolvedReceiver{
				"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
			}

			srv := httptest.NewServer(newReceiverMux(receivers))
			defer srv.Close()

			resp := putBackupFile(t, srv.URL, id, "instance-a", "k", tt.rawQuery, tt.bound)
			_ = resp.Body.Close()

			if resp.StatusCode != tt.expected {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.expected)
			}
		})
	}
}

func TestRemoteTargetInteropWithReceiver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config.Config{Key: "backup-20260101.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	const content = "encrypted backup bytes"

	if err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("pipeline.UploadToRemote() unexpected error: %v", err)
	}

	got, err := os.ReadFile(dir + "/backup-20260101.gpg") //nolint:gosec // dir is t.TempDir() plus a fixed test literal
	if err != nil {
		t.Fatalf("reading object the receiver wrote: %v", err)
	}

	if string(got) != content {
		t.Errorf("received content = %q, want %q", got, content)
	}
}

func TestRemoteTargetInteropWithCreationTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	createdAt := time.Now().Add(-48 * time.Hour).Truncate(time.Second).UTC()
	cfg := &config.Config{Key: "backup-20260101.gpg", CreatedAt: createdAt}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("pipeline.UploadToRemote() unexpected error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "backup-20260101.gpg"))
	if err != nil {
		t.Fatalf("stat-ing received object: %v", err)
	}

	if !info.ModTime().Equal(createdAt) {
		t.Errorf("received object mtime = %v, want %v (cfg.CreatedAt)", info.ModTime(), createdAt)
	}
}

func TestRemoteTargetInteropWithoutCreationTimeUsesUploadTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	before := time.Now().Add(-time.Minute)
	cfg := &config.Config{Key: "backup-20260101.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("pipeline.UploadToRemote() unexpected error: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "backup-20260101.gpg"))
	if err != nil {
		t.Fatalf("stat-ing received object: %v", err)
	}

	if info.ModTime().Before(before) {
		t.Errorf("received object mtime = %v, want at or after %v (upload time, unmodified)", info.ModTime(), before)
	}
}

func TestRemoteTargetInteropFutureCreationTimeRejected(t *testing.T) {
	t.Parallel()

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	resp := putWithRawQuery(t, srv.URL, id, "instance-a", "backup.gpg", "creationTime="+url.QueryEscape(future))
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (creationTime in the future)", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestRemoteTargetInteropMalformedCreationTimeRejected(t *testing.T) {
	t.Parallel()

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	resp := putWithRawQuery(t, srv.URL, id, "instance-a", "backup.gpg", "creationTime=not-a-time")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (malformed creationTime)", resp.StatusCode, http.StatusBadRequest)
	}
}

// putWithRawQuery sends an authenticated (signed with id, matching the
// receiver's configured public key) PUT to baseURL/api/v1/objects/recvID/key
// with rawQuery attached as-is, for tests exercising HandleReceiveObject's
// creationTime validation (which runs after authorization, so a valid
// Authorization header is required to reach it).
func putWithRawQuery(t *testing.T, baseURL string, id *identity.ServerIdentity, recvID, key, rawQuery string) *http.Response {
	t.Helper()

	token, err := id.SignRequest(recvID, remoteauth.Request{})
	if err != nil {
		t.Fatalf("signing request: %v", err)
	}

	u := baseURL + "/api/v1/objects/" + recvID + "/" + key + "?" + rawQuery

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, u, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sending request: %v", err)
	}

	return resp
}

func TestRemoteTargetInteropWrongIdentity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: dir},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	// cfg signs with a different identity than the one the receiver trusts,
	// so its signature won't verify against the receiver's configured
	// public-key:.
	id := testServerIdentity(t)
	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("pipeline.UploadToRemote() with the wrong identity error = %v, want it to mention 401", err)
	}
}

func TestRemoteTargetInteropNoAuthorizationHeader(t *testing.T) {
	t.Parallel()

	_, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
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
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: t.TempDir()},
	}

	srv := httptest.NewServer(newReceiverMux(receivers))
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "no-such-id"}

	err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("pipeline.UploadToRemote() with unknown id error = %v, want it to mention 404", err)
	}
}

// newReceiverMuxWithDB is newReceiverMux, but wiring db through to
// HandleReceiveObject instead of nil, so a
// test can inspect what they recorded to it (retention tracking,
// receiver_events).
func newReceiverMuxWithDB(receivers map[string]config.ResolvedReceiver, db *store.Store) *http.ServeMux {
	receiverStatus := status.NewReceiverStore(receivers)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", HandleReceiveObject(backup.NewReceiverRegistry(receivers), receiverStatus, discardLogger, db))

	return mux
}

func TestHandleReceiveObjectRecordsReceiverEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: filepath.Join(dir, "objects")},
	}

	srv := httptest.NewServer(newReceiverMuxWithDB(receivers, db))
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	const content = "ciphertext bytes"

	if err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader(content)); err != nil {
		t.Fatalf("pipeline.UploadToRemote() error: %v", err)
	}

	summaries, err := db.SummarizeReceiverEvents(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
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

	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	// A plain file where the receiver's root directory should be: writing
	// "backup.gpg" under it needs os.MkdirAll(root) to succeed, and MkdirAll
	// fails when root already exists as a non-directory.
	root := filepath.Join(dir, "not-a-directory")
	writeFile(t, root, "occupied")

	id, key := testServerIdentityAndKey(t)
	receivers := map[string]config.ResolvedReceiver{
		"instance-a": {ID: "instance-a", PublicKey: &key.PublicKey, Path: root},
	}

	srv := httptest.NewServer(newReceiverMuxWithDB(receivers, db))
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "instance-a"}

	if err := pipeline.UploadToRemote(t.Context(), id, cfg, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("pipeline.UploadToRemote() error = nil, want a failure since the receiver's root isn't a directory")
	}

	errs, err := db.ListReceiverErrorEvents(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReadReceiverErrorEvents() error: %v", err)
	}

	if len(errs) != 1 || errs[0].ReceiverID != "instance-a" || errs[0].Kind != store.ReceiverEventReceive {
		t.Fatalf("ReadReceiverErrorEvents() = %+v, want one failed receive for instance-a", errs)
	}
}
