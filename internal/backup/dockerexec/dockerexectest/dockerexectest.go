// Package dockerexectest is a fake Docker daemon for tests: just enough of
// the Engine API's exec endpoints, served on a Unix socket, to exercise
// dockerexec and its callers without a real daemon.
package dockerexectest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Exec is one exec the fake daemon was asked to create.
type Exec struct {
	Container string
	User      string
	Cmd       []string
	Env       []string
}

// Daemon is a running fake daemon.
type Daemon struct {
	// Socket is the Unix socket path to point dockerexec.New at.
	Socket string

	// Containers maps each container the daemon knows (by name) to its
	// behavior; any other name gets a 404 "No such container".
	Containers map[string]Container

	mu    sync.Mutex
	execs []Exec
}

// Container scripts what an exec in it does: it writes Stdout and Stderr
// (as separate multiplexed frames) and exits with ExitCode. Stopped makes
// creating an exec fail with a 409, as for a container that isn't running.
type Container struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Stopped  bool
}

// Execs returns every exec created so far.
func (d *Daemon) Execs() []Exec {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]Exec(nil), d.execs...)
}

// Start serves a fake daemon on a fresh Unix socket until the test ends.
func Start(t *testing.T, containers map[string]Container) *Daemon {
	t.Helper()

	// t.TempDir paths can exceed the ~104-byte Unix socket path limit on
	// macOS, so use a short directory of our own.
	dir, err := os.MkdirTemp("", "dockerd") //nolint:usetesting // t.TempDir's path is too long for a socket on macOS
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	d := &Daemon{Socket: filepath.Join(dir, "d.sock"), Containers: containers}

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", d.Socket)
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: d.handler()} //nolint:gosec // test-only server on a private socket
	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return d
}

func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1.41/containers/{name}/exec", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")

		c, ok := d.Containers[name]
		if !ok {
			writeError(w, http.StatusNotFound, "No such container: "+name)
			return
		}

		if c.Stopped {
			writeError(w, http.StatusConflict, fmt.Sprintf("Container %s is not running", name))
			return
		}

		var body struct {
			Cmd  []string
			Env  []string
			User string
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		d.mu.Lock()
		d.execs = append(d.execs, Exec{Container: name, User: body.User, Cmd: body.Cmd, Env: body.Env})
		d.mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": "exec-" + name})
	})

	mux.HandleFunc("POST /v1.41/exec/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		c := d.Containers[strings.TrimPrefix(r.PathValue("id"), "exec-")]

		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(http.StatusOK)
		writeFrame(w, 1, c.Stdout)
		writeFrame(w, 2, c.Stderr)
	})

	mux.HandleFunc("GET /v1.41/exec/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		c := d.Containers[strings.TrimPrefix(r.PathValue("id"), "exec-")]
		_ = json.NewEncoder(w).Encode(map[string]any{"ExitCode": c.ExitCode, "Running": false})
	})

	return mux
}

func writeFrame(w http.ResponseWriter, stream byte, payload string) {
	if payload == "" {
		return
	}

	header := [8]byte{stream}
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload))) //nolint:gosec // test payloads are tiny
	_, _ = w.Write(header[:])
	_, _ = w.Write([]byte(payload))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
