package receiver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteclient"
	"nilswitt.dev/go-backup-tool/internal/backup/status"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
	"nilswitt.dev/go-backup-tool/internal/backup/trust"
)

type testManager struct {
	*Manager

	registry *Registry
	status   *status.ReceiverStore
	db       store.Store
	trusted  *trust.Registry
	baseDir  string
}

func newTestManager(t *testing.T, db store.Store) testManager {
	t.Helper()

	registry := NewRegistry(nil)
	receiverStatus := status.NewReceiverStore(nil)
	baseDir := t.TempDir()
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops": {ID: "ops", Webhook: &notify.Webhook{URL: "http://example.invalid"}}})
	trusted := trust.NewRegistry(map[string]trust.Server{testSenderUUID: {ID: testSenderUUID, Name: "sender"}})

	return testManager{
		Manager:  NewManager(db, registry, receiverStatus, notifications, trusted, "host", baseDir, discardLogger),
		registry: registry, status: receiverStatus, db: db, trusted: trusted, baseDir: baseDir,
	}
}

func TestManagerCreateUpdateDelete(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	ctx := t.Context()

	fr := config.FileReceiver{ID: " a ", AllowedServers: []string{testSenderUUID}, Path: filepath.Join(m.baseDir, "a"), Retention: "30d"}
	if err := m.Create(ctx, "erin", fr); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	checkManagerActive(t, m, "a")

	if err := m.Create(ctx, "erin", fr); !errors.Is(err, store.ErrReceiverExists) {
		t.Errorf("duplicate Create() error = %v, want ErrReceiverExists", err)
	}

	fr.ID = "a"
	fr.StaleAfter, fr.StaleNotifications = "1d", []string{"ops"}

	if err := m.Update(ctx, "frank", fr); err != nil {
		t.Fatalf("Update() error: %v", err)
	}

	if recv, _ := m.registry.Get("a"); recv.StaleAfter == 0 || len(recv.StaleNotifications) != 1 {
		t.Errorf("after Update, registry a = %+v, want stale monitoring set", recv)
	}

	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].CreatedBy != "erin" || list[0].UpdatedBy != "frank" {
		t.Errorf("List() = %+v, %v", list, err)
	}

	checkManagerDelete(t, m, "a")
}

// checkManagerActive checks receiver id, just created through m, is in the
// registry (resolved, with m's server name) and the status store.
func checkManagerActive(t *testing.T, m testManager, id string) {
	t.Helper()

	recv, ok := m.registry.Get(id)
	if !ok || recv.ServerName != "host" || recv.Retention == 0 {
		t.Fatalf("registry %s = %+v, %v, want it active with retention and server name", id, recv, ok)
	}

	if snaps := m.status.Snapshot(); len(snaps) != 1 || snaps[0].ID != id {
		t.Errorf("status = %+v, want just %s", snaps, id)
	}
}

// checkManagerDelete deletes active receiver id through m and checks it's
// gone from the registry and status store, and that deleting it again
// reports it missing.
func checkManagerDelete(t *testing.T, m testManager, id string) {
	t.Helper()

	ctx := t.Context()

	if err := m.Delete(ctx, "frank", id); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	if _, ok := m.registry.Get(id); ok {
		t.Errorf("%s still in registry after Delete()", id)
	}

	if snaps := m.status.Snapshot(); len(snaps) != 0 {
		t.Errorf("status after Delete() = %+v, want empty", snaps)
	}

	if err := m.Delete(ctx, "frank", id); !errors.Is(err, store.ErrReceiverNotFound) {
		t.Errorf("second Delete() error = %v, want ErrReceiverNotFound", err)
	}
}

func TestManagerDirectoryReceiverTypeIsFixed(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	ctx := t.Context()

	fr := config.FileReceiver{ID: "docs", Type: "directory", KeepVersions: 3, AllowedServers: []string{testSenderUUID}, Path: filepath.Join(m.baseDir, "docs")}
	if err := m.Create(ctx, "erin", fr); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if recv, _ := m.registry.Get("docs"); recv.Kind != config.ReceiverKindDirectory || recv.KeepVersions != 3 {
		t.Errorf("registry docs = %+v, want a directory receiver keeping 3 versions", recv)
	}

	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].FileReceiver().Type != "directory" || list[0].FileReceiver().KeepVersions != 3 {
		t.Errorf("List() = %+v, %v, want the type and keep-versions stored", list, err)
	}

	fr.KeepVersions = 5
	if err := m.Update(ctx, "erin", fr); err != nil {
		t.Errorf("Update(keep-versions) error: %v", err)
	}

	fr.Type, fr.KeepVersions = "", 0
	if err := m.Update(ctx, "erin", fr); !errors.Is(err, ErrInvalidReceiver) {
		t.Errorf("Update(type change) error = %v, want ErrInvalidReceiver", err)
	}
}

func TestManagerRejectsInvalidReceivers(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	inside := filepath.Join(m.baseDir, "a")

	for name, fr := range map[string]config.FileReceiver{
		"path outside base dir":  {ID: "a", AllowedServers: []string{testSenderUUID}, Path: t.TempDir()},
		"path escaping base dir": {ID: "a", AllowedServers: []string{testSenderUUID}, Path: m.baseDir + "/../x"},
		"unknown notification":   {ID: "a", AllowedServers: []string{testSenderUUID}, Path: inside, DownloadNotifications: []string{"missing"}},
		"stale without notify":   {ID: "a", AllowedServers: []string{testSenderUUID}, Path: inside, StaleAfter: "1d"},
		"bad retention":          {ID: "a", AllowedServers: []string{testSenderUUID}, Path: inside, Retention: "soon"},
	} {
		if err := m.Create(t.Context(), "erin", fr); !errors.Is(err, ErrInvalidReceiver) {
			t.Errorf("%s: Create() error = %v, want ErrInvalidReceiver", name, err)
		}
	}

	if len(m.registry.Snapshot()) != 0 {
		t.Error("an invalid receiver made it into the registry")
	}
}

// TestManagerLoadResolvesStoredReceivers checks that stored receivers are
// activated, that one imported from an older config file keeps a path
// outside the base dir editable as long as it's unchanged, and that a
// stored receiver that no longer resolves is reported rather than
// activated.
func TestManagerLoadResolvesStoredReceivers(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()
	legacyPath := t.TempDir()

	seedReceivers(t, db,
		store.ReceiverConfig{ID: "broken", AllowedServers: []string{testSenderUUID}, Path: legacyPath, DownloadNotifications: []string{"removed"}},
		// Imported from a pre-v8 config file's receivers:, with a path
		// outside webui.receivers-base-dir; such rows stay valid as stored.
		store.ReceiverConfig{ID: "legacy", AllowedServers: []string{testSenderUUID}, Path: legacyPath, CreatedBy: "config file", UpdatedBy: "config file"},
	)

	m := newTestManager(t, db)
	m.Load(ctx)

	if _, ok := m.registry.Get("legacy"); !ok {
		t.Fatal("stored receiver legacy not active")
	}

	if _, ok := m.registry.Get("broken"); ok {
		t.Error("unresolvable receiver broken is active")
	}

	list, err := m.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List() = %+v, %v, want broken and legacy", list, err)
	}

	if list[0].ID != "broken" || list[0].Error == "" || list[1].CreatedBy != "config file" {
		t.Errorf("List() = %+v, want broken with an error and legacy from the config file", list)
	}

	if err := m.Update(ctx, "erin", config.FileReceiver{ID: "legacy", AllowedServers: []string{testSenderUUID}, Path: legacyPath, Retention: "7d"}); err != nil {
		t.Errorf("Update() keeping the legacy path error: %v", err)
	}

	if err := m.Update(ctx, "erin", config.FileReceiver{ID: "legacy", AllowedServers: []string{testSenderUUID}, Path: t.TempDir()}); !errors.Is(err, ErrInvalidReceiver) {
		t.Errorf("Update() moving to another path outside the base dir error = %v, want ErrInvalidReceiver", err)
	}

	if err := m.Update(ctx, "erin", config.FileReceiver{ID: "broken", AllowedServers: []string{testSenderUUID}, Path: legacyPath}); err != nil {
		t.Fatalf("fixing broken via Update() error: %v", err)
	}

	if _, ok := m.registry.Get("broken"); !ok {
		t.Error("fixed receiver broken not active")
	}
}

func TestManagerWithoutStateDB(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)

	m.Load(t.Context())

	if len(m.registry.Snapshot()) != 0 {
		t.Error("receivers active without a state db, want none")
	}

	err := m.Create(t.Context(), "erin", config.FileReceiver{ID: "b", AllowedServers: []string{testSenderUUID}, Path: filepath.Join(m.baseDir, "b")})
	if !errors.Is(err, ErrReceiverStoreUnavailable) {
		t.Errorf("Create() without a state db error = %v, want ErrReceiverStoreUnavailable", err)
	}
}

// TestReceiverCreatedAtRuntimeAcceptsUploads checks that a receiver created
// through the Manager is served by the receiver API immediately, and
// rejected again once deleted — no restart needed.
func TestReceiverCreatedAtRuntimeAcceptsUploads(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	id, key := testServerIdentityAndKey(t)
	m.trusted.Put(trust.Server{ID: testSenderUUID, Name: "sender", PublicKey: &key.PublicKey})

	mux := http.NewServeMux()
	RegisterRoutes(mux, m.registry, m.status, discardLogger, m.db)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg"}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "late"}

	if err := remoteclient.Upload(t.Context(), id, cfg, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("upload to a not-yet-created receiver succeeded")
	}

	path := filepath.Join(m.baseDir, "late")
	if err := m.Create(t.Context(), "erin", config.FileReceiver{ID: "late", AllowedServers: []string{testSenderUUID}, Path: path}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := remoteclient.Upload(t.Context(), id, cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("upload after Create() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(path, "backup.gpg")); err != nil {
		t.Errorf("uploaded object missing: %v", err)
	}

	if err := m.Delete(t.Context(), "erin", "late"); err != nil {
		t.Fatal(err)
	}

	if err := remoteclient.Upload(t.Context(), id, cfg, tgt, strings.NewReader("x")); err == nil {
		t.Error("upload after Delete() succeeded")
	}
}

// seedReceivers stores rcs directly, bypassing the manager's validation.
func seedReceivers(t *testing.T, db store.Store, rcs ...store.ReceiverConfig) {
	t.Helper()

	for _, rc := range rcs {
		if err := db.CreateReceiverConfig(t.Context(), rc); err != nil {
			t.Fatalf("CreateReceiverConfig(%q) error: %v", rc.ID, err)
		}
	}
}

// testSenderUUID is the server id every test identity uses (see
// testServerIdentityAndKey), and so the trusted server test receivers allow.
const testSenderUUID = "test-sender-uuid"
