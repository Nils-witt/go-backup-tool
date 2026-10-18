package objectstore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// plainHash is the hex SHA-256 of s, as an on-change job sends it.
func plainHash(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// writeVersion stores content (standing in for its ciphertext too) as rel's
// newest version under root, failing the test on error.
func writeVersion(t *testing.T, root, rel, content string) (int, bool) {
	t.Helper()

	version, stored, err := WriteFileVersion(root, rel, plainHash(content), strings.NewReader(content))
	if err != nil {
		t.Fatalf("WriteFileVersion(%q, %q) error: %v", rel, content, err)
	}

	return version, stored
}

func TestValidateFilePath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{name: "nested", rel: "docs/a.txt"},
		{name: "dot file", rel: ".bashrc"},
		{name: "traversal", rel: "../a", wantErr: true},
		{name: "absolute", rel: "/etc/passwd", wantErr: true},
		{name: "reserved file suffix", rel: "a.gbtv", wantErr: true},
		{name: "reserved dir suffix", rel: "x.gbtv/a", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := ValidateFilePath(tc.rel); (err != nil) != tc.wantErr {
				t.Errorf("ValidateFilePath(%q) error = %v, wantErr %v", tc.rel, err, tc.wantErr)
			}
		})
	}
}

func TestWriteFileVersion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	steps := []struct {
		content     string
		wantVersion int
		wantStored  bool
	}{
		{content: "one", wantVersion: 1, wantStored: true},
		{content: "one", wantVersion: 1, wantStored: false},
		{content: "two", wantVersion: 2, wantStored: true},
		{content: "one", wantVersion: 3, wantStored: true}, // a revert is a change from the newest
	}

	for i, s := range steps {
		version, stored := writeVersion(t, root, "docs/a.txt", s.content)
		if version != s.wantVersion || stored != s.wantStored {
			t.Errorf("step %d: WriteFileVersion(%q) = (%d, %v), want (%d, %v)", i, s.content, version, stored, s.wantVersion, s.wantStored)
		}
	}

	got, err := os.ReadFile(filepath.Join(root, "docs", "a.txt.gbtv", "000002-"+plainHash("two")+".gpg")) //nolint:gosec // root is t.TempDir() plus fixed test literals
	if err != nil || string(got) != "two" {
		t.Errorf("version 2 = %q, %v; want \"two\"", got, err)
	}

	if _, _, err := WriteFileVersion(root, "docs/a.txt", "not-a-hash", strings.NewReader("x")); err == nil {
		t.Error("WriteFileVersion() with a malformed hash succeeded, want an error")
	}
}

func TestWriteFileVersionConcurrent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	const writers = 8

	var wg sync.WaitGroup

	for i := range writers {
		wg.Go(func() {
			content := strings.Repeat("x", i+1)
			if _, _, err := WriteFileVersion(root, "a", plainHash(content), strings.NewReader(content)); err != nil {
				t.Errorf("WriteFileVersion() error: %v", err)
			}
		})
	}

	wg.Wait()

	versions, err := ListFileVersions(root)
	if err != nil {
		t.Fatalf("ListFileVersions() error: %v", err)
	}

	if len(versions) != writers {
		t.Fatalf("ListFileVersions() = %d versions, want %d (no number reused)", len(versions), writers)
	}

	for i, v := range versions {
		if v.Version != i+1 {
			t.Errorf("versions[%d].Version = %d, want %d", i, v.Version, i+1)
		}
	}
}

func TestListFileVersions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	writeVersion(t, root, "b.txt", "b1")
	writeVersion(t, root, "a/c.txt", "c1")
	writeVersion(t, root, "a/c.txt", "c2")

	// A leftover temp file from an interrupted write isn't a version.
	if err := os.WriteFile(filepath.Join(root, "b.txt.gbtv", ".123.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ListFileVersions(root)
	if err != nil {
		t.Fatalf("ListFileVersions() error: %v", err)
	}

	want := []struct {
		path    string
		version int
		key     string
	}{
		{path: "a/c.txt", version: 1, key: "a/c.txt.gbtv/000001-" + plainHash("c1") + ".gpg"},
		{path: "a/c.txt", version: 2, key: "a/c.txt.gbtv/000002-" + plainHash("c2") + ".gpg"},
		{path: "b.txt", version: 1, key: "b.txt.gbtv/000001-" + plainHash("b1") + ".gpg"},
	}

	if len(got) != len(want) {
		t.Fatalf("ListFileVersions() = %+v, want %d versions", got, len(want))
	}

	for i, w := range want {
		if g := got[i]; g.Path != w.path || g.Version != w.version || g.Key != w.key || g.Size != 2 {
			t.Errorf("versions[%d] = %+v, want path %q version %d key %q size 2", i, g, w.path, w.version, w.key)
		}
	}

	if empty, err := ListFileVersions(filepath.Join(root, "missing")); err != nil || len(empty) != 0 {
		t.Errorf("ListFileVersions(missing root) = %v, %v; want empty, nil", empty, err)
	}
}

func TestSweepFileVersions(t *testing.T) {
	t.Parallel()

	now := time.Now()
	day := 24 * time.Hour

	// Five versions of one file, received 50, 40, 30, 20 and 10 days ago.
	ages := []time.Duration{50 * day, 40 * day, 30 * day, 20 * day, 10 * day}

	for _, tc := range []struct {
		name   string
		policy VersionRetention
		want   []int // versions left
	}{
		{name: "no rules keeps everything", policy: VersionRetention{}, want: []int{1, 2, 3, 4, 5}},
		{name: "versions only", policy: VersionRetention{KeepVersions: 2}, want: []int{4, 5}},
		{name: "age only", policy: VersionRetention{MaxAge: 35 * day}, want: []int{3, 4, 5}},
		{name: "both, versions keep more", policy: VersionRetention{KeepVersions: 4, MaxAge: 25 * day}, want: []int{2, 3, 4, 5}},
		{name: "both, age keeps more", policy: VersionRetention{KeepVersions: 1, MaxAge: 45 * day}, want: []int{2, 3, 4, 5}},
		{name: "newest always kept", policy: VersionRetention{MaxAge: day}, want: []int{5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()

			for i, age := range ages {
				content := strings.Repeat("v", i+1)
				version, _ := writeVersion(t, root, "f", content)

				p := filepath.Join(root, "f.gbtv", versionFileName(version, plainHash(content)))
				if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
					t.Fatal(err)
				}
			}

			removed, err := SweepFileVersions(root, tc.policy, now, discardLogger)
			if err != nil {
				t.Fatalf("SweepFileVersions() error: %v", err)
			}

			versions, err := ListFileVersions(root)
			if err != nil {
				t.Fatalf("ListFileVersions() error: %v", err)
			}

			got := make([]int, len(versions))
			for i, v := range versions {
				got[i] = v.Version
			}

			if !slices.Equal(got, tc.want) || removed != len(ages)-len(tc.want) {
				t.Errorf("after sweep: versions %v (removed %d), want %v", got, removed, tc.want)
			}
		})
	}
}
