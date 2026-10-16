package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// writeTree creates files (relative path -> content) under a new temp dir
// named "data" and returns that dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "data")

	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

// readArchive returns the archive's entries: name -> content ("" for
// non-regular entries), plus each symlink's target under "->name".
func readArchive(t *testing.T, r io.Reader, gzipped bool) map[string]string {
	t.Helper()

	if gzipped {
		zr, err := gzip.NewReader(r)
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}

		r = zr
	}

	entries := map[string]string{}
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}

		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}

		entries[hdr.Name] = string(body)

		if hdr.Typeflag == tar.TypeSymlink {
			entries["->"+hdr.Name] = hdr.Linkname
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

func TestValidatePattern(t *testing.T) {
	t.Parallel()

	if err := ValidatePattern("*.log"); err != nil {
		t.Errorf("ValidatePattern(*.log) = %v, want nil", err)
	}

	if err := ValidatePattern("[a-"); !errors.Is(err, path.ErrBadPattern) {
		t.Errorf("ValidatePattern([a-) = %v, want ErrBadPattern", err)
	}
}

func TestWrite(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"a.txt":           "alpha",
		"sub/b.txt":       "beta",
		"sub/skip.log":    "noise",
		"cache/c.bin":     "cached",
		"keep/cache.txt":  "kept",
		"sub/deep/d.conf": "delta",
	}

	tests := []struct {
		name     string
		exclude  []string
		gzip     bool
		expected []string
	}{
		{
			name:     "whole tree",
			expected: []string{"data/", "data/a.txt", "data/cache/", "data/cache/c.bin", "data/keep/", "data/keep/cache.txt", "data/sub/", "data/sub/b.txt", "data/sub/deep/", "data/sub/deep/d.conf", "data/sub/skip.log"},
		},
		{
			name:     "gzip",
			gzip:     true,
			expected: []string{"data/", "data/a.txt", "data/cache/", "data/cache/c.bin", "data/keep/", "data/keep/cache.txt", "data/sub/", "data/sub/b.txt", "data/sub/deep/", "data/sub/deep/d.conf", "data/sub/skip.log"},
		},
		{
			name:     "exclude by base name and relative dir path",
			exclude:  []string{"*.log", "cache", "sub/deep"},
			expected: []string{"data/", "data/a.txt", "data/keep/", "data/keep/cache.txt", "data/sub/", "data/sub/b.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := writeTree(t, files)

			var buf bytes.Buffer
			if err := Write(t.Context(), &buf, Options{Root: root, Exclude: tt.exclude, Gzip: tt.gzip}); err != nil {
				t.Fatalf("Write: %v", err)
			}

			entries := readArchive(t, &buf, tt.gzip)
			if got := sortedKeys(entries); !slices.Equal(got, tt.expected) {
				t.Fatalf("entries = %v, want %v", got, tt.expected)
			}

			if entries["data/a.txt"] != "alpha" {
				t.Errorf("data/a.txt = %q, want %q", entries["data/a.txt"], "alpha")
			}
		})
	}
}

func TestWriteSingleFile(t *testing.T) {
	t.Parallel()

	root := writeTree(t, map[string]string{"dump.sql": "select 1;"})

	var buf bytes.Buffer
	if err := Write(t.Context(), &buf, Options{Root: filepath.Join(root, "dump.sql")}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries := readArchive(t, &buf, false)
	if len(entries) != 1 || entries["dump.sql"] != "select 1;" {
		t.Errorf("entries = %v, want just dump.sql", entries)
	}
}

func TestWriteSymlinks(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs extra privileges on Windows")
	}

	root := writeTree(t, map[string]string{"a.txt": "alpha"})

	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	// A symlinked root is archived as its target, under the link's name.
	rootLink := filepath.Join(filepath.Dir(root), "current")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(t.Context(), &buf, Options{Root: rootLink}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries := readArchive(t, &buf, false)
	if entries["current/a.txt"] != "alpha" {
		t.Errorf("current/a.txt = %q, want %q (entries %v)", entries["current/a.txt"], "alpha", sortedKeys(entries))
	}

	if entries["->current/link"] != "a.txt" {
		t.Errorf("current/link target = %q, want %q", entries["->current/link"], "a.txt")
	}
}

func TestWriteErrors(t *testing.T) {
	t.Parallel()

	root := writeTree(t, map[string]string{"a.txt": "alpha"})

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context //nolint:containedctx // test table input
		opts     Options
		expected error
	}{
		{name: "missing root", ctx: t.Context(), opts: Options{Root: filepath.Join(root, "missing")}, expected: os.ErrNotExist},
		{name: "bad pattern", ctx: t.Context(), opts: Options{Root: root, Exclude: []string{"[a-"}}, expected: path.ErrBadPattern},
		{name: "canceled", ctx: canceled, opts: Options{Root: root}, expected: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := Write(tt.ctx, io.Discard, tt.opts); !errors.Is(err, tt.expected) {
				t.Errorf("Write() = %v, want %v", err, tt.expected)
			}
		})
	}
}
