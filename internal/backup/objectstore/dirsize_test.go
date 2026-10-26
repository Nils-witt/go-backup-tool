package objectstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

func TestDirSize(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}

	for path, size := range map[string]int{"top.gpg": 10, "a/one.gpg": 5, "a/b/two.gpg": 7} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := DirSize(root); err != nil || got != 22 {
		t.Errorf("DirSize() = (%d, %v), want (22, nil)", got, err)
	}

	if got, err := DirSize(filepath.Join(root, "missing")); err != nil || got != 0 {
		t.Errorf("DirSize(missing) = (%d, %v), want (0, nil)", got, err)
	}
}

func TestWriteNewLocalObject(t *testing.T) {
	t.Parallel()

	tgt := &config.Target{LocalPath: t.TempDir(), Bucket: "b"}
	cfg := &config.Config{Key: "k.gpg"}

	if err := WriteNewLocalObject(cfg, tgt, strings.NewReader("first")); err != nil {
		t.Fatalf("first WriteNewLocalObject() unexpected error: %v", err)
	}

	if err := WriteNewLocalObject(cfg, tgt, strings.NewReader("second")); !errors.Is(err, ErrObjectExists) {
		t.Fatalf("second WriteNewLocalObject() error = %v, want ErrObjectExists", err)
	}

	dir := filepath.Dir(LocalObjectPath(cfg, tgt))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("object dir holds %d entries, want only the object (no leftover temp file)", len(entries))
	}

	if got, err := os.ReadFile(LocalObjectPath(cfg, tgt)); err != nil || string(got) != "first" {
		t.Errorf("object = (%q, %v), want the first write untouched", got, err)
	}
}
