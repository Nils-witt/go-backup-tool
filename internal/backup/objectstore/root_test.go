package objectstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// symlinkedReceiver returns a receiver base holding a symlink "out" to
// another directory, which holds secret.gpg, and that directory.
func symlinkedReceiver(t *testing.T) (base, outside string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs extra privileges on Windows")
	}

	base, outside = t.TempDir(), t.TempDir()

	if err := os.WriteFile(filepath.Join(outside, "secret.gpg"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(base, "out")); err != nil {
		t.Fatal(err)
	}

	return base, outside
}

// TestSymlinksDontLeaveTheReceiver checks that a symlink inside a
// receiver's directory can't lead a write, read, listing, or retention
// delete outside it.
func TestSymlinksDontLeaveTheReceiver(t *testing.T) {
	t.Parallel()

	base, outside := symlinkedReceiver(t)
	tgt := &config.Target{ServerName: "receiver:r", Kind: config.ServerKindLocal, LocalPath: base, Retention: time.Hour}

	if err := WriteLocalObject(&config.Config{Key: "out/new.gpg"}, tgt, strings.NewReader("data")); err == nil {
		t.Error("WriteLocalObject() through a symlink succeeded, want an error")
	}

	if _, err := os.Stat(filepath.Join(outside, "new.gpg")); err == nil {
		t.Error("WriteLocalObject() wrote outside the receiver")
	}

	if f, err := OpenObject(base, "out/secret.gpg"); err == nil {
		_ = f.Close()

		t.Error("OpenObject() through a symlink succeeded, want an error")
	}

	files, err := ListReceiverFiles(config.ResolvedReceiver{ID: "r", Path: base})
	if err != nil || len(files) != 0 {
		t.Errorf("ListReceiverFiles() = %+v, %v; want nothing listed through the symlink", files, err)
	}

	if size, err := DirSize(base); err != nil || size != 0 {
		t.Errorf("DirSize() = %d, %v; want 0", size, err)
	}

	db := openTestRetentionDB(t)
	planted := filepath.Join(base, "out", "secret.gpg")

	if err := db.SaveObjectWrite(t.Context(), tgt.ServerName, "", planted, time.Now().Add(-2*time.Hour), 3600); err != nil {
		t.Fatal(err)
	}

	_ = SweepRetentionForTarget(t.Context(), db, tgt, discardLogger)

	if _, err := os.Stat(filepath.Join(outside, "secret.gpg")); err != nil {
		t.Errorf("retention sweep deleted a file outside the receiver: %v", err)
	}
}

// TestFileVersionsDontFollowSymlinks checks the same for a directory
// receiver's versions.
func TestFileVersionsDontFollowSymlinks(t *testing.T) {
	t.Parallel()

	base, outside := symlinkedReceiver(t)
	sum := strings.Repeat("a", 64)

	if _, _, err := WriteFileVersion(base, "out/doc.txt", sum, strings.NewReader("data")); err == nil {
		t.Error("WriteFileVersion() through a symlink succeeded, want an error")
	}

	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("outside directory holds %d entries, want only secret.gpg", len(entries))
	}
}
