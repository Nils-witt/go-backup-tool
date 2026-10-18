package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestJobConfigCreateAndUpdate(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)

	j := JobConfig{
		Name: "db", Command: "dump", Key: "db-{time}.gpg", Recipients: []string{"me@example.com"}, Armor: true,
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

func TestJobConfigDelete(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.CreateJobConfig(ctx, JobConfig{Name: "db", Command: "edited"}); err != nil {
		t.Fatalf("CreateJobConfig() error: %v", err)
	}

	if err := db.CreateJobConfig(ctx, JobConfig{Name: "files", Command: "tar"}); err != nil {
		t.Fatalf("CreateJobConfig() error: %v", err)
	}

	if err := db.DeleteJobConfig(ctx, "db"); err != nil {
		t.Fatalf("DeleteJobConfig() error: %v", err)
	}

	if err := db.DeleteJobConfig(ctx, "db"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("second DeleteJobConfig() error = %v, want ErrJobNotFound", err)
	}

	all, err := db.ListJobConfigs(ctx)
	if err != nil || len(all) != 1 || all[0].Name != "files" || all[0].Command != "tar" {
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

func TestServerConfigServerUUID(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	remote := ServerConfig{Name: "sibling", Type: "remote", Endpoint: "https://x", ServerUUID: "6f1c2a9e-3b4d-4e5f-8a7b-1c2d3e4f5a6b"}
	if err := db.CreateServerConfig(ctx, remote); err != nil {
		t.Fatalf("CreateServerConfig() error: %v", err)
	}

	remote.ServerUUID = "0d4e5f6a-7b8c-4d9e-8f0a-1b2c3d4e5f6a"
	if err := db.UpdateServerConfig(ctx, remote); err != nil {
		t.Fatalf("UpdateServerConfig() error: %v", err)
	}

	servers, err := db.ListServerConfigs(ctx)
	if err != nil || len(servers) != 1 || servers[0].ServerUUID != remote.ServerUUID {
		t.Errorf("ListServerConfigs() = %+v, %v", servers, err)
	}
}

// serverTLSOf is sc's TLS settings, for comparing them at once.
func serverTLSOf(sc ServerConfig) [3]any {
	return [3]any{sc.TLSInsecureSkipVerify, sc.TLSSkipHostnameVerify, sc.TLSCA}
}

// onlyServerTLS returns the TLS settings stored for server name.
func onlyServerTLS(t *testing.T, db *gormStore, name string) [3]any {
	t.Helper()

	servers, err := db.ListServerConfigs(t.Context())
	if err != nil {
		t.Fatalf("ListServerConfigs() error: %v", err)
	}

	for _, sc := range servers {
		if sc.Name == name {
			return serverTLSOf(sc)
		}
	}

	t.Fatalf("ListServerConfigs() = %+v, want server %q", servers, name)

	return [3]any{}
}

func TestServerConfigTLS(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	remote := ServerConfig{Name: "sibling", Type: "remote", Endpoint: "https://x", TLSSkipHostnameVerify: true, TLSCA: "-----BEGIN CERTIFICATE-----"}
	if err := db.CreateServerConfig(ctx, remote); err != nil {
		t.Fatalf("CreateServerConfig() error: %v", err)
	}

	if got := onlyServerTLS(t, db, "sibling"); got != serverTLSOf(remote) {
		t.Errorf("TLS after create = %v, want %v", got, serverTLSOf(remote))
	}

	// Switching to insecure must also clear the other two: false and ""
	// are written, not skipped as zero values.
	remote.TLSInsecureSkipVerify, remote.TLSSkipHostnameVerify, remote.TLSCA = true, false, ""
	if err := db.UpdateServerConfig(ctx, remote); err != nil {
		t.Fatalf("UpdateServerConfig() error: %v", err)
	}

	if got := onlyServerTLS(t, db, "sibling"); got != serverTLSOf(remote) {
		t.Errorf("TLS after update = %v, want %v", got, serverTLSOf(remote))
	}

	other := ServerConfig{Name: "other", Type: "remote", Endpoint: "https://y", TLSInsecureSkipVerify: true}
	if err := db.CreateServerConfig(ctx, other); err != nil {
		t.Fatalf("CreateServerConfig() error: %v", err)
	}

	if got := onlyServerTLS(t, db, "other"); got != serverTLSOf(other) {
		t.Errorf("TLS after create = %v, want %v", got, serverTLSOf(other))
	}
}

func TestCommandConfigCRUD(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	if err := db.CreateCommandConfig(ctx, CommandConfig{ID: "page", Cmd: "echo", Timeout: "5s"}); err != nil {
		t.Fatalf("CreateCommandConfig() error: %v", err)
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

func TestCommandConfigTarFields(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	tarCmd := CommandConfig{ID: "etc", Type: "tar", Path: "/etc", Exclude: []string{"*.bak"}, Compress: "gzip"}
	if err := db.CreateCommandConfig(ctx, tarCmd); err != nil {
		t.Fatalf("CreateCommandConfig() error: %v", err)
	}

	// Switching it back to a shell command must clear every tar field.
	if err := db.UpdateCommandConfig(ctx, CommandConfig{ID: "etc", Cmd: "cat /etc/hosts"}); err != nil {
		t.Fatalf("UpdateCommandConfig() error: %v", err)
	}

	if err := db.CreateCommandConfig(ctx, CommandConfig{ID: "www", Type: "tar", Path: "/srv"}); err != nil {
		t.Fatalf("CreateCommandConfig() error: %v", err)
	}

	tarCmd.Path = "/etc/nginx"
	tarCmd.ID = "www"

	if err := db.UpdateCommandConfig(ctx, tarCmd); err != nil {
		t.Fatalf("UpdateCommandConfig() error: %v", err)
	}

	commands, err := db.ListCommandConfigs(ctx)
	if err != nil {
		t.Fatalf("ListCommandConfigs() error: %v", err)
	}

	want := []CommandConfig{
		{ID: "etc", Cmd: "cat /etc/hosts"},
		{ID: "www", Type: "tar", Path: "/etc/nginx", Exclude: []string{"*.bak"}, Compress: "gzip"},
	}

	for i := range commands {
		commands[i].CreatedAt, commands[i].UpdatedAt = time.Time{}, time.Time{}
	}

	// etc's exclude reads back as JSON null or []; either is "no excludes".
	if len(commands) > 0 && len(commands[0].Exclude) == 0 {
		commands[0].Exclude = nil
	}

	if !reflect.DeepEqual(commands, want) {
		t.Errorf("ListCommandConfigs() = %+v, want %+v", commands, want)
	}
}
