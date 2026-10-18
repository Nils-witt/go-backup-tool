package config

import (
	"strings"
	"testing"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

func TestOnChangeSourceExcluded(t *testing.T) {
	t.Parallel()

	src := OnChangeSource{Exclude: []string{"*.tmp", "cache"}}

	for _, tc := range []struct {
		name string
		rel  string
		want bool
	}{
		{name: "base name match", rel: "a/b.tmp", want: true},
		{name: "directory match", rel: "cache", want: true},
		{name: "no match", rel: "a/b.txt", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := src.Excluded(tc.rel); got != tc.want {
				t.Errorf("Excluded(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestResolveJobOnChange(t *testing.T) {
	t.Parallel()

	remote, err := ResolveServer(FileServer{Name: "offsite", Type: "remote", Endpoint: "https://offsite.example.com"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	job, err := ResolveJob(FileJob{
		Name:       "docs",
		Type:       "on-change",
		Source:     &FileJobSource{Path: "/srv/docs/", Exclude: []string{"*.tmp"}, Watch: true, Debounce: "10s"},
		Recipients: []string{"me@example.com"},
		Targets:    []FileJobTarget{{Server: "offsite", Bucket: "docs"}},
	}, notify.GPGSettings{Bin: "gpg"}, map[string]ResolvedServer{"offsite": remote}, nil, "")
	if err != nil {
		t.Fatalf("ResolveJob() error: %v", err)
	}

	want := OnChangeSource{Path: "/srv/docs", Exclude: []string{"*.tmp"}, Watch: true, Debounce: 10 * time.Second}
	if job.Kind != JobKindOnChange || job.Source.Path != want.Path || job.Source.Debounce != want.Debounce || !job.Source.Watch {
		t.Errorf("ResolveJob() = kind %q source %+v, want on-change %+v", job.Kind, job.Source, want)
	}
}

func TestResolveJobOnChangeDefaultDebounce(t *testing.T) {
	t.Parallel()

	remote, err := ResolveServer(FileServer{Name: "offsite", Type: "remote", Endpoint: "https://offsite.example.com"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	job, err := ResolveJob(FileJob{
		Name:       "docs",
		Type:       "on-change",
		Source:     &FileJobSource{Path: "/srv/docs"},
		Interval:   "1h",
		Recipients: []string{"me@example.com"},
		Targets:    []FileJobTarget{{Server: "offsite", Bucket: "docs"}},
	}, notify.GPGSettings{Bin: "gpg"}, map[string]ResolvedServer{"offsite": remote}, nil, "")
	if err != nil {
		t.Fatalf("ResolveJob() error: %v", err)
	}

	if job.Source.Debounce != DefaultOnChangeDebounce {
		t.Errorf("Source.Debounce = %v, want %v", job.Source.Debounce, DefaultOnChangeDebounce)
	}
}

func TestResolveJobOnChangeErrors(t *testing.T) {
	t.Parallel()

	local, err := ResolveServer(FileServer{Name: "nas", Type: "local", Path: "/mnt"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	remote, err := ResolveServer(FileServer{Name: "offsite", Type: "remote", Endpoint: "https://offsite.example.com"})
	if err != nil {
		t.Fatalf("ResolveServer() error: %v", err)
	}

	cmd, err := ResolveCommand(FileCommand{ID: "page", Cmd: "echo page"})
	if err != nil {
		t.Fatalf("ResolveCommand() error: %v", err)
	}

	servers := map[string]ResolvedServer{"nas": local, "offsite": remote}
	commands := map[string]Command{"page": cmd}
	toRemote := []FileJobTarget{{Server: "offsite", Bucket: "b"}}
	src := &FileJobSource{Path: "/srv", Watch: true}

	for _, tc := range []struct {
		name string
		fj   FileJob
		want string
	}{
		{name: "unknown type", fj: FileJob{Name: "x", Type: "sometimes"}, want: `unknown type "sometimes"`},
		{name: "no source", fj: FileJob{Name: "x", Type: "on-change", Recipients: []string{"r"}, Targets: toRemote}, want: "source.path is required"},
		{name: "relative path", fj: FileJob{Name: "x", Type: "on-change", Source: &FileJobSource{Path: "srv", Watch: true}}, want: "must be absolute"},
		{name: "bad exclude", fj: FileJob{Name: "x", Type: "on-change", Source: &FileJobSource{Path: "/srv", Exclude: []string{"["}}}, want: "exclude pattern"},
		{name: "bad debounce", fj: FileJob{Name: "x", Type: "on-change", Source: &FileJobSource{Path: "/srv", Debounce: "0s"}}, want: "debounce must be positive"},
		{name: "files set", fj: FileJob{Name: "x", Type: "on-change", Source: src, Files: []FileJobFile{{Command: "page"}}, Recipients: []string{"r"}, Targets: toRemote}, want: "files is not valid"},
		{name: "local target", fj: FileJob{Name: "x", Type: "on-change", Source: src, Recipients: []string{"r"}, Targets: []FileJobTarget{{Server: "nas", Bucket: "b"}}}, want: "only to remote"},
		{name: "no watch or interval", fj: FileJob{Name: "x", Type: "on-change", Source: &FileJobSource{Path: "/srv"}, Recipients: []string{"r"}, Targets: toRemote}, want: "needs source.watch"},
		{name: "source on backup job", fj: FileJob{Name: "x", Source: src}, want: "only valid for an on-change job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := ResolveJob(tc.fj, notify.GPGSettings{Bin: "gpg"}, servers, commands, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ResolveJob() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestResolveReceiverKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		fr       FileReceiver
		wantKind ReceiverKind
		wantErr  string
	}{
		{name: "default backup", fr: FileReceiver{}, wantKind: ReceiverKindBackup},
		{name: "directory with versions", fr: FileReceiver{Type: "directory", KeepVersions: 3}, wantKind: ReceiverKindDirectory},
		{name: "unknown type", fr: FileReceiver{Type: "bucket"}, wantErr: `unknown type "bucket"`},
		{name: "negative versions", fr: FileReceiver{Type: "directory", KeepVersions: -1}, wantErr: "must not be negative"},
		{name: "versions on backup receiver", fr: FileReceiver{KeepVersions: 2}, wantErr: "only valid for a directory receiver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fr := tc.fr
			fr.ID, fr.AllowedServers, fr.Path = "a", []string{testSenderID}, "/mnt/a"

			got, err := ResolveReceiver(fr, nil, nil, "")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ResolveReceiver() error = %v, want substring %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("ResolveReceiver() error: %v", err)
			}

			if got.Kind != tc.wantKind || got.KeepVersions != fr.KeepVersions {
				t.Errorf("ResolveReceiver() = kind %q keep %d, want %q %d", got.Kind, got.KeepVersions, tc.wantKind, fr.KeepVersions)
			}
		})
	}
}
