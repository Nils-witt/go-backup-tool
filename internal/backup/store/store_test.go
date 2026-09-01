package store

import (
	"path/filepath"
	"testing"
)

func TestStateDBPath(t *testing.T) {
	t.Parallel()

	got := StateDBPath("/etc/go-backup-tool/config.yaml")
	want := filepath.Join("/etc/go-backup-tool", stateDBName)

	if got != want {
		t.Errorf("StateDBPath() = %q, want %q", got, want)
	}
}
