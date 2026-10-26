package objectstore

import (
	"os"
	"path/filepath"
	"testing"
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
