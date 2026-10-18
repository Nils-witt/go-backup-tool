package receiver

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/objectstore"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
)

// sha256Hex is the hex SHA-256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// newDirectoryReceiverServer serves a directory receiver (or, with kind
// backup, a backup receiver) at dir that trusts the returned identity.
func newDirectoryReceiverServer(t *testing.T, kind config.ReceiverKind, dir string) (*httptest.Server, *identity.ServerIdentity, *config.Target) {
	t.Helper()

	id, key := testServerIdentityAndKey(t)
	recv := config.ResolvedReceiver{
		ID: "files", Kind: kind, AllowedServers: []string{testSenderUUID},
		TrustedServers: trustSender(&key.PublicKey), Path: dir,
	}

	srv := httptest.NewServer(newReceiverMux(map[string]config.ResolvedReceiver{recv.ID: recv}))
	t.Cleanup(srv.Close)

	return srv, id, &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: recv.ID}
}

func TestRemoteTargetInteropFileVersions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, id, tgt := newDirectoryReceiverServer(t, config.ReceiverKindDirectory, dir)

	// The bodies stand in for ciphertext; the receiver only sees the
	// plaintext's hash.
	steps := []struct {
		plain       string
		wantStored  bool
		wantVersion int
	}{
		{plain: "v1", wantStored: true, wantVersion: 1},
		{plain: "v1", wantStored: false, wantVersion: 1},
		{plain: "v2", wantStored: true, wantVersion: 2},
	}

	for i, s := range steps {
		res, err := remoteclient.UploadFile(t.Context(), id, tgt, "docs/my file.txt", sha256Hex(s.plain), strings.NewReader("cipher of "+s.plain))
		if err != nil {
			t.Fatalf("step %d: UploadFile() error: %v", i, err)
		}

		if res.Stored != s.wantStored || res.Version != s.wantVersion {
			t.Errorf("step %d: UploadFile() = %+v, want stored %v version %d", i, res, s.wantStored, s.wantVersion)
		}
	}

	versions, err := objectstore.ListFileVersions(dir)
	if err != nil {
		t.Fatalf("ListFileVersions() error: %v", err)
	}

	if len(versions) != 2 || versions[0].Path != "docs/my file.txt" || versions[1].SHA256 != sha256Hex("v2") {
		t.Errorf("ListFileVersions() = %+v, want two versions of docs/my file.txt", versions)
	}
}

func TestHandleReceiveFileWrongReceiverKind(t *testing.T) {
	t.Parallel()

	t.Run("file upload to a backup receiver", func(t *testing.T) {
		t.Parallel()

		_, id, tgt := newDirectoryReceiverServer(t, config.ReceiverKindBackup, t.TempDir())

		_, err := remoteclient.UploadFile(t.Context(), id, tgt, "a.txt", sha256Hex("a"), strings.NewReader("x"))
		if err == nil || !strings.Contains(err.Error(), "409") {
			t.Errorf("UploadFile() error = %v, want a 409", err)
		}
	})

	t.Run("backup upload to a directory receiver", func(t *testing.T) {
		t.Parallel()

		_, id, tgt := newDirectoryReceiverServer(t, config.ReceiverKindDirectory, t.TempDir())

		err := remoteclient.Upload(t.Context(), id, &config.Config{Key: "backup-1"}, tgt, strings.NewReader("x"))
		if err == nil || !strings.Contains(err.Error(), "409") {
			t.Errorf("Upload() error = %v, want a 409", err)
		}
	})
}

func TestHandleReceiveFileRejectsBadTokens(t *testing.T) {
	t.Parallel()

	const body = "x"

	plain := sha256Hex("plain")
	good := remoteauth.Request{Method: http.MethodPut, Key: "a.txt", BodySHA256: sha256Hex(body), PlainSHA256: plain}

	tests := []struct {
		name     string
		route    string
		query    string
		bound    remoteauth.Request
		expected int
	}{
		{name: "valid", route: "files/files/a.txt", query: "sha256=" + plain, bound: good, expected: http.StatusCreated},
		{name: "unbound token", route: "files/files/a.txt", query: "sha256=" + plain, bound: remoteauth.Request{}, expected: http.StatusUnauthorized},
		{name: "other path", route: "files/files/b.txt", query: "sha256=" + plain, bound: good, expected: http.StatusUnauthorized},
		{name: "other content hash", route: "files/files/a.txt", query: "sha256=" + sha256Hex("other"), bound: good, expected: http.StatusUnauthorized},
		{name: "reserved suffix", route: "files/files/a.gbtv", query: "sha256=" + plain, bound: good, expected: http.StatusBadRequest},
		{name: "traversal", route: "files/files/..%2Fescape", query: "sha256=" + plain, bound: good, expected: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, id, _ := newDirectoryReceiverServer(t, config.ReceiverKindDirectory, t.TempDir())

			resp := putSigned(t, srv.URL+"/api/v1/"+tc.route+"?"+tc.query, id, "files", body, tc.bound)
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.expected {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.expected)
			}
		})
	}
}

func TestHandleReceiveObjectRejectsFileUploadToken(t *testing.T) {
	t.Parallel()

	const body = "x"

	srv, id, _ := newDirectoryReceiverServer(t, config.ReceiverKindBackup, t.TempDir())

	// A token minted for a file upload, replayed against a backup
	// receiver's object route with the same key and body.
	bound := remoteauth.Request{Method: http.MethodPut, Key: "a.txt", BodySHA256: sha256Hex(body), PlainSHA256: sha256Hex("plain")}

	resp := putSigned(t, srv.URL+"/api/v1/objects/files/a.txt", id, "files", body, bound)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// putSigned sends body as a PUT to u, with a token for recvID signed with
// id for bound.
func putSigned(t *testing.T, u string, id *identity.ServerIdentity, recvID, body string, bound remoteauth.Request) *http.Response {
	t.Helper()

	token, err := id.SignRequest(recvID, bound)
	if err != nil {
		t.Fatalf("signing request: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, u, strings.NewReader(body))
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
