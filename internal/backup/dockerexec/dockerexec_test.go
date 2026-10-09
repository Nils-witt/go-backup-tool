package dockerexec_test

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec"
	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec/dockerexectest"
)

func newClient(t *testing.T, containers map[string]dockerexectest.Container) (*dockerexec.Client, *dockerexectest.Daemon) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake daemon listens on a Unix socket")
	}

	d := dockerexectest.Start(t, containers)

	c, err := dockerexec.New("unix://" + d.Socket)
	if err != nil {
		t.Fatal(err)
	}

	return c, d
}

func TestRunDemultiplexesOutput(t *testing.T) {
	t.Parallel()

	c, d := newClient(t, map[string]dockerexectest.Container{
		"db": {Stdout: "dump data", Stderr: "a warning\n"},
	})

	var stdout, stderr bytes.Buffer

	err := c.Run(context.Background(), dockerexec.Exec{
		Container: "db",
		User:      "postgres",
		Cmd:       []string{"sh", "-c", "pg_dump app"},
		Env:       []string{"A=1"},
		Stdout:    &stdout,
		Stderr:    &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stdout.String() != "dump data" || stderr.String() != "a warning\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	execs := d.Execs()
	if len(execs) != 1 {
		t.Fatalf("got %d execs, want 1", len(execs))
	}

	if e := execs[0]; e.User != "postgres" || !slices.Equal(e.Cmd, []string{"sh", "-c", "pg_dump app"}) || !slices.Equal(e.Env, []string{"A=1"}) {
		t.Errorf("exec = %+v", e)
	}
}

func TestRunNonzeroExit(t *testing.T) {
	t.Parallel()

	c, _ := newClient(t, map[string]dockerexectest.Container{"db": {ExitCode: 3}})

	err := c.Run(context.Background(), dockerexec.Exec{Container: "db", Cmd: []string{"false"}})

	var exitErr *dockerexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
}

func TestRunReportsDaemonErrors(t *testing.T) {
	t.Parallel()

	c, _ := newClient(t, map[string]dockerexectest.Container{"db": {Stopped: true}})

	for name, want := range map[string]string{
		"db":      "is not running",
		"missing": "No such container: missing",
	} {
		err := c.Run(context.Background(), dockerexec.Exec{Container: name, Cmd: []string{"true"}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("container %q: err = %v, want it to mention %q", name, err, want)
		}
	}
}

func TestNewRejectsUnknownScheme(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"npipe:////./pipe/docker_engine", "docker.sock", "tcp://"} {
		if _, err := dockerexec.New(host); err == nil {
			t.Errorf("New(%q) succeeded, want an error", host)
		}
	}
}

func TestResolveHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")

	if got := dockerexec.ResolveHost(""); got != dockerexec.DefaultSocket {
		t.Errorf("ResolveHost(\"\") = %q, want the default socket", got)
	}

	t.Setenv("DOCKER_HOST", "tcp://docker:2375")

	if got := dockerexec.ResolveHost(""); got != "tcp://docker:2375" {
		t.Errorf("ResolveHost(\"\") = %q, want DOCKER_HOST", got)
	}

	if got := dockerexec.ResolveHost(" /run/podman.sock "); got != "/run/podman.sock" {
		t.Errorf("ResolveHost = %q, want the configured socket", got)
	}
}
