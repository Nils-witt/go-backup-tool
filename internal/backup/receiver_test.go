package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func TestSanitizeObjectKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key     string
		wantErr bool
	}{
		{key: "backup-20260101.gpg", wantErr: false},
		{key: "subdir/backup-20260101.gpg", wantErr: false},
		{key: "", wantErr: true},
		{key: "/etc/passwd", wantErr: true},
		{key: "..", wantErr: true},
		{key: "../etc/passwd", wantErr: true},
		{key: "subdir/../../etc/passwd", wantErr: true},
		{key: "subdir//double-slash", wantErr: true},
		{key: ".", wantErr: true},
	}

	for _, tt := range tests {
		got, err := SanitizeObjectKey(tt.key)
		if (err != nil) != tt.wantErr {
			t.Errorf("SanitizeObjectKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			continue
		}

		if err == nil && got != tt.key {
			t.Errorf("SanitizeObjectKey(%q) = %q, want unchanged", tt.key, got)
		}
	}
}

func TestListReceiverFilesMissingRoot(t *testing.T) {
	t.Parallel()

	recv := config.ResolvedReceiver{ID: "a", Path: filepath.Join(t.TempDir(), "does-not-exist")}

	files, err := ListReceiverFiles(recv)
	if err != nil {
		t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("ListReceiverFiles() = %+v, want empty", files)
	}
}

func TestListReceiverFilesListsNestedObjectsSortedByCreatedTimeAscending(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	newerPath := filepath.Join(root, "b.gpg")
	olderPath := filepath.Join(root, "subdir", "a.gpg")

	writeFile(t, newerPath, "bbb")
	writeFile(t, olderPath, "a")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	older := time.Now().Add(-time.Hour)
	newer := time.Now()

	if err := os.Chtimes(olderPath, older, older); err != nil {
		t.Fatalf("os.Chtimes(%q) unexpected error: %v", olderPath, err)
	}

	if err := os.Chtimes(newerPath, newer, newer); err != nil {
		t.Fatalf("os.Chtimes(%q) unexpected error: %v", newerPath, err)
	}

	recv := config.ResolvedReceiver{ID: "a", Path: root}

	files, err := ListReceiverFiles(recv)
	if err != nil {
		t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("ListReceiverFiles() = %+v, want 2 entries", files)
	}

	if files[0].Key != "subdir/a.gpg" || files[0].Size != 1 {
		t.Errorf("files[0] = %+v, want key subdir/a.gpg size 1 (oldest)", files[0])
	}

	if files[1].Key != "b.gpg" || files[1].Size != 3 {
		t.Errorf("files[1] = %+v, want key b.gpg size 3 (newest)", files[1])
	}
}

// writeFile creates path (and any missing parent directories) with contents.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating directory for %q: %v", path, err)
	}

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
}

func TestLastReceivedAtMissingRoot(t *testing.T) {
	t.Parallel()

	recv := config.ResolvedReceiver{ID: "a", Path: filepath.Join(t.TempDir(), "does-not-exist")}

	_, ok, err := LastReceivedAt(recv)
	if err != nil {
		t.Fatalf("LastReceivedAt() unexpected error: %v", err)
	}

	if ok {
		t.Error("LastReceivedAt() ok = true, want false for a missing root")
	}
}

func TestLastReceivedAtReturnsNewestModTime(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	older := filepath.Join(root, "older.gpg")
	newer := filepath.Join(root, "subdir", "newer.gpg")

	writeFile(t, older, "a")
	writeFile(t, newer, "b")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	olderTime := time.Now().Add(-2 * time.Hour)
	newerTime := time.Now().Add(-1 * time.Hour)

	if err := os.Chtimes(older, olderTime, olderTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", older, err)
	}

	if err := os.Chtimes(newer, newerTime, newerTime); err != nil {
		t.Fatalf("Chtimes(%q): %v", newer, err)
	}

	// A hidden temp file, if it were counted, would appear newest of all
	// (its default mtime is "now"), so this also verifies it's skipped.
	got, ok, err := LastReceivedAt(config.ResolvedReceiver{ID: "a", Path: root})
	if err != nil {
		t.Fatalf("LastReceivedAt() unexpected error: %v", err)
	}

	if !ok {
		t.Fatal("LastReceivedAt() ok = false, want true")
	}

	if !got.Equal(newerTime) {
		t.Errorf("LastReceivedAt() = %v, want %v", got, newerTime)
	}
}

func TestReceiverStatusStoreSeedLastEventSuccess(t *testing.T) {
	t.Parallel()

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := NewReceiverStatusStore(receivers)

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SeedLastEvent("a", "obj.gpg", at, true, "")

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %+v, want 1 entry", snap)
	}

	if snap[0].State != StateOK || snap[0].LastKey != "obj.gpg" || !snap[0].LastSeen.Equal(at) || snap[0].Error != "" {
		t.Errorf("snapshot()[0] = %+v, want state ok, last_key obj.gpg, last_seen %v, no error", snap[0], at)
	}
}

func TestReceiverStatusStoreSeedLastEventFailure(t *testing.T) {
	t.Parallel()

	receivers := map[string]config.ResolvedReceiver{"a": {ID: "a", Path: t.TempDir()}}
	store := NewReceiverStatusStore(receivers)

	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	store.SeedLastEvent("a", "obj.gpg", at, false, "disk full")

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %+v, want 1 entry", snap)
	}

	if snap[0].State != StateFailed || snap[0].Error != "disk full" {
		t.Errorf("snapshot()[0] = %+v, want state failed with error %q", snap[0], "disk full")
	}
}

func TestReceiverStatusStoreSeedLastEventUnknownReceiverIsNoop(t *testing.T) {
	t.Parallel()

	store := NewReceiverStatusStore(map[string]config.ResolvedReceiver{})

	store.SeedLastEvent("does-not-exist", "", time.Time{}, true, "")

	if snap := store.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot() = %+v, want none", snap)
	}
}
