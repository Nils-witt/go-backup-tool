package pipeline

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/dockerexec/dockerexectest"
)

// fakeGPG writes a stand-in gpg that ignores its arguments and copies stdin
// to stdout, so a test can see exactly what the source command produced.
func fakeGPG(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "gpg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec cat\n"), 0o700); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}

	return path
}

func skipOnWindows(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("needs sh and a Unix socket")
	}
}

func TestContainerSourceFeedsGPG(t *testing.T) {
	t.Parallel()

	skipOnWindows(t)

	d := dockerexectest.Start(t, map[string]dockerexectest.Container{
		"db": {Stdout: "dump from the container", Stderr: "notice\n"},
	})

	cfg := &config.Config{
		Cmd: "pg_dump app", Container: "db", ContainerUser: "postgres",
		DockerHost: d.Socket, GPGBin: fakeGPG(t),
	}

	source, gpgCmd, gpgOut, err := startEncryptingPipeline(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("startEncryptingPipeline: %v", err)
	}

	out, err := io.ReadAll(gpgOut)
	if err != nil {
		t.Fatal(err)
	}

	if err := gpgCmd.Wait(); err != nil {
		t.Fatalf("gpg: %v", err)
	}

	if err := source.Wait(); err != nil {
		t.Fatalf("source: %v", err)
	}

	if string(out) != "dump from the container" {
		t.Errorf("gpg got %q", out)
	}

	execs := d.Execs()
	if len(execs) != 1 || execs[0].User != "postgres" || !slices.Equal(execs[0].Cmd, []string{"sh", "-c", "pg_dump app"}) {
		t.Errorf("execs = %+v", execs)
	}
}

func TestContainerSourceFailureEndsGPGInput(t *testing.T) {
	t.Parallel()

	skipOnWindows(t)

	d := dockerexectest.Start(t, nil)
	cfg := &config.Config{Cmd: "pg_dump app", Container: "gone", DockerHost: d.Socket, GPGBin: fakeGPG(t)}

	source, gpgCmd, gpgOut, err := startEncryptingPipeline(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("startEncryptingPipeline: %v", err)
	}

	// gpg must see EOF rather than wait forever on a source that never ran.
	_, _ = io.Copy(io.Discard, gpgOut)
	_ = gpgCmd.Wait()

	err = source.Wait()
	if err == nil || !strings.Contains(err.Error(), "No such container: gone") {
		t.Fatalf("source err = %v, want the daemon's error", err)
	}

	if got := firstPipelineError(sourceLabel(cfg), err, nil, nil, nil).Error(); !strings.Contains(got, `in container "gone" failed`) {
		t.Errorf("pipeline error %q doesn't name the container", got)
	}
}

func TestRunTargetCommandInContainer(t *testing.T) {
	t.Parallel()

	skipOnWindows(t)

	d := dockerexectest.Start(t, map[string]dockerexectest.Container{"app": {}})
	target := &config.Target{ServerName: "nas", Bucket: "db"}
	cmd := config.Command{ID: "restart", Cmd: "touch /tmp/failed", Container: "app", ContainerUser: "root"}

	runTargetCommand(context.Background(), d.Socket, "on-error", "nightly", target, cmd, 2, io.ErrUnexpectedEOF, slog.New(slog.DiscardHandler))

	execs := d.Execs()
	if len(execs) != 1 {
		t.Fatalf("got %d execs, want 1", len(execs))
	}

	e := execs[0]
	if e.Container != "app" || e.User != "root" || !slices.Equal(e.Cmd, []string{"sh", "-c", "touch /tmp/failed"}) {
		t.Errorf("exec = %+v", e)
	}

	for _, want := range []string{"GBT_EVENT=error", "GBT_JOB=nightly", "GBT_SERVER=nas", "GBT_TARGET=db", "GBT_CONSECUTIVE_FAILURES=2", "GBT_ERROR=unexpected EOF"} {
		if !slices.Contains(e.Env, want) {
			t.Errorf("env %v lacks %q", e.Env, want)
		}
	}

	// Only the GBT_* variables go in, never this process's environment.
	for _, kv := range e.Env {
		if !strings.HasPrefix(kv, "GBT_") {
			t.Errorf("unexpected env %q passed into the container", kv)
		}
	}
}
