package receiver

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/notify"
	"nilswitt.dev/go-backup-tool/internal/backup/pipeline"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

func publicKeyPEM(t *testing.T, key *rsa.PublicKey) string {
	t.Helper()

	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatalf("marshaling public key: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

type testManager struct {
	*Manager

	registry *backup.ReceiverRegistry
	status   *backup.ReceiverStatusStore
	db       *store.Store
	baseDir  string
}

func newTestManager(t *testing.T, db *store.Store) testManager {
	t.Helper()

	registry := backup.NewReceiverRegistry(nil)
	status := backup.NewReceiverStatusStore(nil)
	baseDir := t.TempDir()
	notifications := notify.NewRegistry(map[string]notify.Notification{"ops": {ID: "ops", Webhook: &notify.Webhook{URL: "http://example.invalid"}}})

	return testManager{
		Manager:  NewManager(db, registry, status, notifications, "host", baseDir, discardLogger),
		registry: registry, status: status, db: db, baseDir: baseDir,
	}
}

func TestManagerCreateUpdateDelete(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	ctx := t.Context()
	_, key := testServerIdentityAndKey(t)
	pemText := publicKeyPEM(t, &key.PublicKey)

	fr := config.FileReceiver{ID: " a ", PublicKey: pemText, Path: filepath.Join(m.baseDir, "a"), Retention: "30d"}
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

func TestManagerRejectsInvalidReceivers(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, openTestStateDB(t))
	_, key := testServerIdentityAndKey(t)
	pemText := publicKeyPEM(t, &key.PublicKey)
	inside := filepath.Join(m.baseDir, "a")

	for name, fr := range map[string]config.FileReceiver{
		"path outside base dir":  {ID: "a", PublicKey: pemText, Path: t.TempDir()},
		"path escaping base dir": {ID: "a", PublicKey: pemText, Path: m.baseDir + "/../x"},
		"bad public key":         {ID: "a", PublicKey: "nope", Path: inside},
		"unknown notification":   {ID: "a", PublicKey: pemText, Path: inside, DownloadNotifications: []string{"missing"}},
		"stale without notify":   {ID: "a", PublicKey: pemText, Path: inside, StaleAfter: "1d"},
		"bad retention":          {ID: "a", PublicKey: pemText, Path: inside, Retention: "soon"},
	} {
		if err := m.Create(t.Context(), "erin", fr); !errors.Is(err, ErrInvalidReceiver) {
			t.Errorf("%s: Create() error = %v, want ErrInvalidReceiver", name, err)
		}
	}

	if len(m.registry.Snapshot()) != 0 {
		t.Error("an invalid receiver made it into the registry")
	}
}

// TestManagerLoadImportsConfigFileReceivers checks that the config file's
// receivers: are imported once, keep a path outside the base dir editable
// as long as it's unchanged, and that a stored receiver that no longer
// resolves is reported rather than activated.
func TestManagerLoadImportsConfigFileReceivers(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()
	_, key := testServerIdentityAndKey(t)
	pemText := publicKeyPEM(t, &key.PublicKey)
	legacyPath := t.TempDir()

	if err := db.CreateReceiverConfig(ctx, store.ReceiverConfig{ID: "broken", PublicKey: pemText, Path: legacyPath, DownloadNotifications: []string{"removed"}}); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, db)
	m.Load(ctx, []config.FileReceiver{{ID: "legacy", PublicKey: pemText, Path: legacyPath}})

	if _, ok := m.registry.Get("legacy"); !ok {
		t.Fatal("imported receiver legacy not active")
	}

	if _, ok := m.registry.Get("broken"); ok {
		t.Error("unresolvable receiver broken is active")
	}

	list, err := m.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List() = %+v, %v, want broken and legacy", list, err)
	}

	if list[0].ID != "broken" || list[0].Error == "" || list[1].CreatedBy != configFileUser {
		t.Errorf("List() = %+v, want broken with an error and legacy from the config file", list)
	}

	if err := m.Update(ctx, "erin", config.FileReceiver{ID: "legacy", PublicKey: pemText, Path: legacyPath, Retention: "7d"}); err != nil {
		t.Errorf("Update() keeping the legacy path error: %v", err)
	}

	if err := m.Update(ctx, "erin", config.FileReceiver{ID: "legacy", PublicKey: pemText, Path: t.TempDir()}); !errors.Is(err, ErrInvalidReceiver) {
		t.Errorf("Update() moving to another path outside the base dir error = %v, want ErrInvalidReceiver", err)
	}

	if err := m.Update(ctx, "erin", config.FileReceiver{ID: "broken", PublicKey: pemText, Path: legacyPath}); err != nil {
		t.Fatalf("fixing broken via Update() error: %v", err)
	}

	if _, ok := m.registry.Get("broken"); !ok {
		t.Error("fixed receiver broken not active")
	}
}

func TestManagerWithoutStateDB(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	_, key := testServerIdentityAndKey(t)
	pemText := publicKeyPEM(t, &key.PublicKey)

	m.Load(t.Context(), []config.FileReceiver{{ID: "legacy", PublicKey: pemText, Path: t.TempDir()}})

	if _, ok := m.registry.Get("legacy"); !ok {
		t.Error("config file receiver not active without a state db")
	}

	err := m.Create(t.Context(), "erin", config.FileReceiver{ID: "b", PublicKey: pemText, Path: filepath.Join(m.baseDir, "b")})
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

	mux := http.NewServeMux()
	RegisterRoutes(mux, m.registry, m.status, discardLogger, m.db)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := &config.Config{Key: "backup.gpg", Identity: id}
	tgt := &config.Target{Kind: config.ServerKindRemote, Endpoint: srv.URL, Bucket: "late"}

	if err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x")); err == nil {
		t.Fatal("upload to a not-yet-created receiver succeeded")
	}

	path := filepath.Join(m.baseDir, "late")
	if err := m.Create(t.Context(), "erin", config.FileReceiver{ID: "late", PublicKey: publicKeyPEM(t, &key.PublicKey), Path: path}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x")); err != nil {
		t.Fatalf("upload after Create() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(path, "backup.gpg")); err != nil {
		t.Errorf("uploaded object missing: %v", err)
	}

	if err := m.Delete(t.Context(), "erin", "late"); err != nil {
		t.Fatal(err)
	}

	if err := pipeline.UploadToRemote(t.Context(), cfg, tgt, strings.NewReader("x")); err == nil {
		t.Error("upload after Delete() succeeded")
	}
}
