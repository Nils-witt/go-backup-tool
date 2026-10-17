package identity

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestEnsureServerUUIDGeneratesValidUUID(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "keys")

	id, err := ensureServerUUID(dir, discardLogger)
	if err != nil {
		t.Fatalf("ensureServerUUID() unexpected error: %v", err)
	}

	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("ensureServerUUID() = %q, not a valid UUID: %v", id, err)
	}
}

func TestEnsureServerUUIDIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "keys")

	first, err := ensureServerUUID(dir, discardLogger)
	if err != nil {
		t.Fatalf("first ensureServerUUID() unexpected error: %v", err)
	}

	for i := range 3 {
		got, err := ensureServerUUID(dir, discardLogger)
		if err != nil {
			t.Fatalf("ensureServerUUID() call %d unexpected error: %v", i, err)
		}

		if got != first {
			t.Errorf("ensureServerUUID() call %d = %q, want %q (unchanged)", i, got, first)
		}
	}
}
