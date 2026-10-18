package store

import (
	"testing"
	"time"
)

func TestOnChangeFiles(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	mod := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, f := range []OnChangeFile{
		{JobName: "docs", Target: "offsite", Path: "b.txt", Size: 2, ModTime: mod, SHA256: "h1", UploadedAt: mod},
		{JobName: "docs", Target: "offsite", Path: "a/c.txt", Size: 3, ModTime: mod, SHA256: "h2", UploadedAt: mod},
		{JobName: "docs", Target: "other", Path: "b.txt", Size: 2, ModTime: mod, SHA256: "h1", UploadedAt: mod},
		{JobName: "photos", Target: "offsite", Path: "b.txt", Size: 9, ModTime: mod, SHA256: "h9", UploadedAt: mod},
	} {
		if err := db.SaveOnChangeFile(ctx, f); err != nil {
			t.Fatalf("SaveOnChangeFile(%+v) error: %v", f, err)
		}
	}

	later := mod.Add(time.Hour)
	if err := db.SaveOnChangeFile(ctx, OnChangeFile{JobName: "docs", Target: "offsite", Path: "b.txt", Size: 5, ModTime: later, SHA256: "h3", UploadedAt: later}); err != nil {
		t.Fatalf("SaveOnChangeFile(update) error: %v", err)
	}

	got, err := db.ListOnChangeFiles(ctx, "docs", "offsite")
	if err != nil {
		t.Fatalf("ListOnChangeFiles() error: %v", err)
	}

	if len(got) != 2 || got[0].Path != "a/c.txt" || got[1].Path != "b.txt" {
		t.Fatalf("ListOnChangeFiles() = %+v, want a/c.txt then b.txt", got)
	}

	if b := got[1]; b.Size != 5 || b.SHA256 != "h3" || !b.ModTime.Equal(later) {
		t.Errorf("updated row = %+v, want size 5, hash h3, mod time %v", b, later)
	}

	if err := db.DeleteOnChangeFiles(ctx, "docs"); err != nil {
		t.Fatalf("DeleteOnChangeFiles() error: %v", err)
	}

	checkOnChangeFileCount(t, db, "docs", "offsite", 0)
	checkOnChangeFileCount(t, db, "docs", "other", 0)
	checkOnChangeFileCount(t, db, "photos", "offsite", 1)
}

// checkOnChangeFileCount checks db records want files for job on target.
func checkOnChangeFileCount(t *testing.T, db *gormStore, job, target string, want int) {
	t.Helper()

	rows, err := db.ListOnChangeFiles(t.Context(), job, target)
	if err != nil {
		t.Fatalf("ListOnChangeFiles(%s, %s) error: %v", job, target, err)
	}

	if len(rows) != want {
		t.Errorf("ListOnChangeFiles(%s, %s) = %d rows, want %d", job, target, len(rows), want)
	}
}
