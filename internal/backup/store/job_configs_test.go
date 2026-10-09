package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestJobConfigCreateAndUpdate(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)

	j := JobConfig{
		Name: "db", Cmd: "dump", Key: "db-{time}.gpg", Recipients: []string{"me@example.com"}, Armor: true,
		Targets:   []JobTarget{{Server: "nas", Bucket: "b", OnError: &JobTargetOnError{Command: "page", After: 2}}},
		Interval:  "1h",
		CreatedAt: now, CreatedBy: "erin", UpdatedAt: now, UpdatedBy: "erin",
	}

	if err := db.CreateJobConfig(ctx, j); err != nil {
		t.Fatalf("CreateJobConfig() error: %v", err)
	}

	if err := db.CreateJobConfig(ctx, j); !errors.Is(err, ErrJobExists) {
		t.Errorf("duplicate CreateJobConfig() error = %v, want ErrJobExists", err)
	}

	// Zero values must be written too, not skipped.
	j.Armor, j.Interval, j.Targets = false, "", []JobTarget{{Server: "nas", Bucket: "c"}}
	j.FailureNotifications = []string{"ops"}
	j.CreatedBy, j.UpdatedBy = "ignored", "frank"

	if err := db.UpdateJobConfig(ctx, j); err != nil {
		t.Fatalf("UpdateJobConfig() error: %v", err)
	}

	got, ok, err := db.GetJobConfig(ctx, "db")
	if err != nil || !ok {
		t.Fatalf("GetJobConfig() = %v, %v", ok, err)
	}

	want := j
	want.CreatedBy = "erin"
	got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("after update = %+v, want %+v", got, want)
	}

	if err := db.UpdateJobConfig(ctx, JobConfig{Name: "missing"}); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("UpdateJobConfig(missing) error = %v, want ErrJobNotFound", err)
	}
}

func TestJobConfigImportAndDelete(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.CreateJobConfig(ctx, JobConfig{Name: "db", Cmd: "edited"}); err != nil {
		t.Fatalf("CreateJobConfig() error: %v", err)
	}

	imported, err := db.ImportJobConfigs(ctx, []JobConfig{{Name: "db", Cmd: "other"}, {Name: "files", Cmd: "tar"}})
	if err != nil || !slices.Equal(imported, []string{"files"}) {
		t.Errorf("ImportJobConfigs() = %v, %v; want [files]", imported, err)
	}

	if err := db.DeleteJobConfig(ctx, "db"); err != nil {
		t.Fatalf("DeleteJobConfig() error: %v", err)
	}

	if err := db.DeleteJobConfig(ctx, "db"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("second DeleteJobConfig() error = %v, want ErrJobNotFound", err)
	}

	all, err := db.ListJobConfigs(ctx)
	if err != nil || len(all) != 1 || all[0].Name != "files" || all[0].Cmd != "tar" {
		t.Errorf("ListJobConfigs() = %+v, %v", all, err)
	}
}

func TestServerConfigCRUD(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.CreateServerConfig(ctx, ServerConfig{Name: "nas", Type: "local", Path: "/mnt", Retention: "7d"}); err != nil {
		t.Fatalf("CreateServerConfig() error: %v", err)
	}

	if err := db.CreateServerConfig(ctx, ServerConfig{Name: "nas"}); !errors.Is(err, ErrServerExists) {
		t.Errorf("duplicate CreateServerConfig() error = %v, want ErrServerExists", err)
	}

	if err := db.UpdateServerConfig(ctx, ServerConfig{Name: "nas", Type: "local", Path: "/srv"}); err != nil {
		t.Fatalf("UpdateServerConfig() error: %v", err)
	}

	servers, err := db.ListServerConfigs(ctx)
	if err != nil || len(servers) != 1 || servers[0].Path != "/srv" || servers[0].Retention != "" {
		t.Errorf("ListServerConfigs() = %+v, %v", servers, err)
	}

	if err := db.DeleteServerConfig(ctx, "gone"); !errors.Is(err, ErrServerNotFound) {
		t.Errorf("DeleteServerConfig(gone) error = %v, want ErrServerNotFound", err)
	}
}

func TestCommandConfigCRUD(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	imported, err := db.ImportCommandConfigs(ctx, []CommandConfig{{ID: "page", Cmd: "echo", Timeout: "5s"}})
	if err != nil || !slices.Equal(imported, []string{"page"}) {
		t.Fatalf("ImportCommandConfigs() = %v, %v", imported, err)
	}

	if err := db.UpdateCommandConfig(ctx, CommandConfig{ID: "page", Cmd: "echo 2"}); err != nil {
		t.Fatalf("UpdateCommandConfig() error: %v", err)
	}

	commands, err := db.ListCommandConfigs(ctx)
	if err != nil || len(commands) != 1 || commands[0].Cmd != "echo 2" || commands[0].Timeout != "" {
		t.Errorf("ListCommandConfigs() = %+v, %v", commands, err)
	}

	if err := db.DeleteCommandConfig(ctx, "page"); err != nil {
		t.Errorf("DeleteCommandConfig() error: %v", err)
	}
}
