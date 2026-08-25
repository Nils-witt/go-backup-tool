package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
		got, err := sanitizeObjectKey(tt.key)
		if (err != nil) != tt.wantErr {
			t.Errorf("sanitizeObjectKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			continue
		}

		if err == nil && got != tt.key {
			t.Errorf("sanitizeObjectKey(%q) = %q, want unchanged", tt.key, got)
		}
	}
}

func TestBuildReceivers(t *testing.T) {
	t.Parallel()

	receivers, err := buildReceivers([]fileReceiver{
		{ID: "a", Token: "tok-a", Path: "/mnt/a"},
		{ID: "b", Token: "tok-b", Path: "/mnt/b", Retention: "7d"},
	})
	if err != nil {
		t.Fatalf("buildReceivers() unexpected error: %v", err)
	}

	want := map[string]resolvedReceiver{
		"a": {id: "a", token: "tok-a", path: "/mnt/a"},
		"b": {id: "b", token: "tok-b", path: "/mnt/b", retention: 7 * 24 * time.Hour},
	}

	if len(receivers) != len(want) {
		t.Fatalf("buildReceivers() = %+v, want %+v", receivers, want)
	}

	for id, w := range want {
		if got := receivers[id]; got != w {
			t.Errorf("buildReceivers()[%q] = %+v, want %+v", id, got, w)
		}
	}
}

func TestBuildReceiversRequiresID(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{Token: "t", Path: "/mnt/a"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing id, got nil")
	}
}

func TestBuildReceiversRequiresPath(t *testing.T) {
	t.Parallel()

	_, err := buildReceivers([]fileReceiver{{ID: "a", Token: "t"}})
	if err == nil {
		t.Fatal("buildReceivers() expected error for missing path, got nil")
	}
}

func TestListReceiverFilesMissingRoot(t *testing.T) {
	t.Parallel()

	recv := resolvedReceiver{id: "a", path: filepath.Join(t.TempDir(), "does-not-exist")}

	files, err := listReceiverFiles(recv)
	if err != nil {
		t.Fatalf("listReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("listReceiverFiles() = %+v, want empty", files)
	}
}

func TestListReceiverFilesListsNestedObjectsSortedByKey(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	writeFile(t, filepath.Join(root, "b.gpg"), "bbb")
	writeFile(t, filepath.Join(root, "subdir", "a.gpg"), "a")
	writeFile(t, filepath.Join(root, ".hidden.tmp"), "temp")

	recv := resolvedReceiver{id: "a", path: root}

	files, err := listReceiverFiles(recv)
	if err != nil {
		t.Fatalf("listReceiverFiles() unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("listReceiverFiles() = %+v, want 2 entries", files)
	}

	if files[0].Key != "b.gpg" || files[0].Size != 3 {
		t.Errorf("files[0] = %+v, want key b.gpg size 3", files[0])
	}

	if files[1].Key != "subdir/a.gpg" || files[1].Size != 1 {
		t.Errorf("files[1] = %+v, want key subdir/a.gpg size 1", files[1])
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
