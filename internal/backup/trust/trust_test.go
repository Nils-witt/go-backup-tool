package trust

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

var discardLogger = slog.New(slog.DiscardHandler)

func testKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshaling public key: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), &key.PublicKey
}

func openTestStateDB(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

func TestFingerprint(t *testing.T) {
	t.Parallel()

	_, a := testKeyPEM(t)
	_, b := testKeyPEM(t)

	fa := Fingerprint(a)
	if !strings.HasPrefix(fa, "SHA256:") || len(fa) != len("SHA256:")+43 {
		t.Errorf("Fingerprint() = %q, want SHA256: plus 43 base64 chars", fa)
	}

	if fa != Fingerprint(a) || fa == Fingerprint(b) {
		t.Error("Fingerprint() isn't stable per key / distinct across keys")
	}
}

func TestManagerCreateUpdate(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()
	registry := NewRegistry(nil)
	m := NewManager(db, registry, discardLogger)
	pemA, keyA := testKeyPEM(t)
	pemB, keyB := testKeyPEM(t)
	id := uuid.NewString()

	// Ids are stored canonical (lowercase), whatever case is entered.
	if err := m.Create(ctx, "erin", Input{ID: " " + strings.ToUpper(id) + " ", Name: " nas ", PublicKey: pemA}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if s, ok := registry.Get(id); !ok || s.Name != "nas" || !s.PublicKey.Equal(keyA) || s.Fingerprint != Fingerprint(keyA) {
		t.Fatalf("registry after Create = %+v, %v", s, ok)
	}

	if err := m.Create(ctx, "erin", Input{ID: id, Name: "dup", PublicKey: pemA}); !errors.Is(err, store.ErrTrustedServerExists) {
		t.Errorf("duplicate Create() error = %v, want ErrTrustedServerExists", err)
	}

	if err := m.Update(ctx, "frank", Input{ID: id, Name: "nas-2", PublicKey: pemB}); err != nil {
		t.Fatalf("Update() error: %v", err)
	}

	if s, _ := registry.Get(id); s.Name != "nas-2" || !s.PublicKey.Equal(keyB) {
		t.Errorf("registry after Update = %+v, want nas-2 with key B", s)
	}

	reloaded := NewRegistry(nil)
	NewManager(db, reloaded, discardLogger).Load(ctx)

	if s, ok := reloaded.Get(id); !ok || s.Name != "nas-2" {
		t.Errorf("Load() into a fresh registry = %+v, %v; want the stored server", s, ok)
	}
}

// TestManagerDeleteInUse checks List reports which receivers allow a
// server, and Delete refuses while any does.
func TestManagerDeleteInUse(t *testing.T) {
	t.Parallel()

	db := openTestStateDB(t)
	ctx := t.Context()
	registry := NewRegistry(nil)
	m := NewManager(db, registry, discardLogger)
	pemText, key := testKeyPEM(t)
	id := uuid.NewString()

	if err := m.Create(ctx, "erin", Input{ID: id, Name: "nas", PublicKey: pemText}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := db.CreateReceiverConfig(ctx, store.ReceiverConfig{ID: "r1", Path: "/srv/r1", AllowedServers: []string{id}}); err != nil {
		t.Fatalf("CreateReceiverConfig() error: %v", err)
	}

	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].CreatedBy != "erin" || list[0].Fingerprint != Fingerprint(key) ||
		!slices.Equal(list[0].UsedBy, []string{"receiver r1"}) {
		t.Fatalf("List() = %+v, %v; want one server by erin with key's fingerprint, used by receiver r1", list, err)
	}

	if err := m.Delete(ctx, "erin", id); !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "receiver r1") {
		t.Errorf("Delete() of an in-use server error = %v, want ErrInUse naming receiver r1", err)
	}

	if err := db.DeleteReceiverConfig(ctx, "r1"); err != nil {
		t.Fatalf("DeleteReceiverConfig() error: %v", err)
	}

	if err := m.Delete(ctx, "erin", id); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	if _, ok := registry.Get(id); ok {
		t.Error("registry still holds the deleted server")
	}
}

func TestManagerValidation(t *testing.T) {
	t.Parallel()

	m := NewManager(openTestStateDB(t), NewRegistry(nil), discardLogger)
	pemText, _ := testKeyPEM(t)

	cases := map[string]Input{
		"not a uuid":  {ID: "nas-01", Name: "nas", PublicKey: pemText},
		"no name":     {ID: uuid.NewString(), Name: " ", PublicKey: pemText},
		"bad key":     {ID: uuid.NewString(), Name: "nas", PublicKey: "nope"},
		"missing key": {ID: uuid.NewString(), Name: "nas"},
	}

	for name, in := range cases {
		if err := m.Create(t.Context(), "erin", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Create() error = %v, want ErrInvalid", name, err)
		}
	}

	if err := m.Update(t.Context(), "erin", Input{ID: uuid.NewString(), Name: "nas", PublicKey: pemText}); !errors.Is(err, store.ErrTrustedServerNotFound) {
		t.Errorf("Update(unknown) error = %v, want ErrTrustedServerNotFound", err)
	}
}

func TestManagerWithoutStateDB(t *testing.T) {
	t.Parallel()

	m := NewManager(nil, NewRegistry(nil), discardLogger)
	m.Load(t.Context())

	if _, err := m.List(t.Context()); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("List() error = %v, want ErrStoreUnavailable", err)
	}

	if err := m.Create(t.Context(), "erin", Input{}); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("Create() error = %v, want ErrStoreUnavailable", err)
	}
}
