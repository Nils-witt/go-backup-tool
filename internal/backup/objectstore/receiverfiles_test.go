package objectstore

import (
	"maps"
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
		{key: `..\..\state.db`, wantErr: true},
		{key: `subdir\..\..\x`, wantErr: true},
		{key: "C:x", wantErr: true},
		{key: "subdir/C:/x", wantErr: true},
		{key: "nul\x00byte", wantErr: true},
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

func TestListReceiverFilesSplitsMultiFileBackups(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	writeFile(t, filepath.Join(root, "host", "backup-1", "db.gpg"), "db")
	writeFile(t, filepath.Join(root, "host", "backup-1", MultiFileMarker), "")
	writeFile(t, filepath.Join(root, "host", "legacy.gpg"), "legacy")

	files, err := ListReceiverFiles(config.ResolvedReceiver{ID: "a", Path: root})
	if err != nil {
		t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
	}

	got := map[string]string{}
	for _, f := range files {
		got[f.Key] = f.File
	}

	expected := map[string]string{"host/backup-1": "db.gpg", "host/legacy.gpg": ""}
	if !maps.Equal(got, expected) {
		t.Errorf("ListReceiverFiles() key -> file = %v, want %v (marker itself hidden)", got, expected)
	}
}

func TestListReceiverFilesExpiresAt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "a.gpg")

	writeFile(t, path, "a")

	t.Run("retention set", func(t *testing.T) {
		t.Parallel()

		recv := config.ResolvedReceiver{ID: "a", Path: root, Retention: 24 * time.Hour}

		files, err := ListReceiverFiles(recv)
		if err != nil {
			t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
		}

		if len(files) != 1 {
			t.Fatalf("ListReceiverFiles() = %+v, want 1 entry", files)
		}

		want := files[0].ModTime.Add(24 * time.Hour)
		if !files[0].ExpiresAt.Equal(want) {
			t.Errorf("files[0].ExpiresAt = %v, want %v", files[0].ExpiresAt, want)
		}
	})

	t.Run("no retention", func(t *testing.T) {
		t.Parallel()

		recv := config.ResolvedReceiver{ID: "a", Path: root}

		files, err := ListReceiverFiles(recv)
		if err != nil {
			t.Fatalf("ListReceiverFiles() unexpected error: %v", err)
		}

		if len(files) != 1 {
			t.Fatalf("ListReceiverFiles() = %+v, want 1 entry", files)
		}

		if !files[0].ExpiresAt.IsZero() {
			t.Errorf("files[0].ExpiresAt = %v, want zero", files[0].ExpiresAt)
		}
	})
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
