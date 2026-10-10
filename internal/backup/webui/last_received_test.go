package webui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func TestLastReceivedCacheReusesWalkUntilLastSeenChanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	recv := config.ResolvedReceiver{ID: "a", Path: root}
	c := &lastReceivedCache{byPath: map[string]lastReceivedEntry{}}

	if _, ok, err := c.get(recv, time.Time{}); err != nil || ok {
		t.Fatalf("get() on empty dir = ok %v, err %v; want false, nil", ok, err)
	}

	if err := os.WriteFile(filepath.Join(root, "obj"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Same lastSeen, within the TTL: the cached "nothing received" stands.
	if _, ok, _ := c.get(recv, time.Time{}); ok {
		t.Error("get() with unchanged lastSeen re-walked, want cached result")
	}

	// A new receive moves lastSeen on: the cache must re-walk.
	if _, ok, _ := c.get(recv, time.Now()); !ok {
		t.Error("get() after lastSeen changed = ok false, want the new file seen")
	}
}
