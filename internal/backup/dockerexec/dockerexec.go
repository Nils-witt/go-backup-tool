// Package dockerexec runs a command inside an already-running container
// through the Docker Engine API (docker exec), talking to the daemon's
// socket directly rather than shelling out to the docker CLI, so neither the
// CLI nor any SDK has to be installed alongside this tool. Podman's
// Docker-compatible socket works too.
package dockerexec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// DefaultSocket is the Docker daemon's default Unix socket, used when
// neither docker-socket: nor DOCKER_HOST names another.
const DefaultSocket = "/var/run/docker.sock"

// apiVersion pins the Engine API version requests are made against: 1.41
// (Docker 20.10, 2020) is old enough for every daemon still in use, Podman
// included, and covers everything exec needs.
const apiVersion = "/v1.41"

// ResolveHost picks the daemon address: host (the docker-socket: setting)
// when set, else DOCKER_HOST, else DefaultSocket.
func ResolveHost(host string) string {
	if h := strings.TrimSpace(host); h != "" {
		return h
	}

	if h := strings.TrimSpace(os.Getenv("DOCKER_HOST")); h != "" {
		return h
	}

	return DefaultSocket
}

// Client talks to one Docker daemon.
type Client struct {
	http *http.Client
	base string
}

// New builds a Client for host: a Unix socket path, either bare
// ("/var/run/docker.sock") or as "unix:///var/run/docker.sock", or a
// "tcp://host:port" daemon address, spoken to as plain HTTP — TLS-protected
// daemons aren't supported.
func New(host string) (*Client, error) {
	transport := &http.Transport{
		// Each exec's output stream ends with the daemon closing the
		// connection (see Run), so connections are never reused.
		DisableKeepAlives: true,
	}

	switch {
	case strings.HasPrefix(host, "tcp://"):
		addr := strings.TrimPrefix(host, "tcp://")
		if addr == "" {
			return nil, fmt.Errorf("docker host %q: missing address", host)
		}

		return &Client{http: &http.Client{Transport: transport}, base: "http://" + addr + apiVersion}, nil

	case strings.HasPrefix(host, "unix://") || strings.HasPrefix(host, "/"):
		path := strings.TrimPrefix(host, "unix://")
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}

		// The host part is ignored by the dialer above but must be valid.
		return &Client{http: &http.Client{Transport: transport}, base: "http://docker" + apiVersion}, nil

	default:
		return nil, fmt.Errorf("docker host %q: must be a socket path, unix://<path>, or tcp://<host>:<port>", host)
	}
}

// Exec is one command to run in a container.
type Exec struct {
	Container string   // container name or id; it must already be running
	User      string   // user[:group] to run as; empty uses the container's default
	Cmd       []string // argv, e.g. {"sh", "-c", "pg_dump app"}
	Env       []string // extra KEY=value pairs on top of the container's own environment

	// Stdout and Stderr receive the command's output; nil discards it.
	Stdout io.Writer
	Stderr io.Writer
}

// ExitError reports a command that ran but exited nonzero, worded like
// os/exec's own *ExitError.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Run runs e to completion, streaming its output to e.Stdout/e.Stderr, and
// returns an *ExitError when it exits nonzero. Canceling ctx stops reading
// its output and returns ctx's error, but the Engine API has no way to kill
// an exec: the process itself keeps running inside the container until it
// exits (or the container stops).
func (c *Client) Run(ctx context.Context, e Exec) error {
	id, err := c.create(ctx, e)
	if err != nil {
		return err
	}

	if err := c.start(ctx, id, e.Stdout, e.Stderr); err != nil {
		return err
	}

	code, err := c.exitCode(ctx, id)
	if err != nil {
		return err
	}

	if code != 0 {
		return &ExitError{Code: code}
	}

	return nil
}

func (c *Client) create(ctx context.Context, e Exec) (string, error) {
	body := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          e.Cmd,
		"Env":          e.Env,
		"User":         e.User,
	}

	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(e.Container)+"/exec", body)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("container %q: %w", e.Container, apiError(resp))
	}

	var created struct {
		ID string `json:"Id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("container %q: reading exec id: %w", e.Container, err)
	}

	if created.ID == "" {
		return "", fmt.Errorf("container %q: no exec id in the daemon's response", e.Container)
	}

	return created.ID, nil
}

// start runs exec id attached, demultiplexing its output stream: without a
// TTY the daemon prefixes every chunk with an 8-byte header — stream type
// (1 stdout, 2 stderr) in byte 0, payload length big-endian in bytes 4-7 —
// and closes the connection once the command exits.
func (c *Client) start(ctx context.Context, id string, stdout, stderr io.Writer) error {
	resp, err := c.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(id)+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("starting exec: %w", apiError(resp))
	}

	if stdout == nil {
		stdout = io.Discard
	}

	if stderr == nil {
		stderr = io.Discard
	}

	var header [8]byte

	for {
		if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("reading exec output: %w", errors.Join(ctx.Err(), err))
		}

		dst := stdout
		if header[0] == 2 {
			dst = stderr
		}

		if _, err := io.CopyN(dst, resp.Body, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return fmt.Errorf("reading exec output: %w", errors.Join(ctx.Err(), err))
		}
	}
}

func (c *Client) exitCode(ctx context.Context, id string) (int, error) {
	resp, err := c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("inspecting exec: %w", apiError(resp))
	}

	var info struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return 0, fmt.Errorf("inspecting exec: %w", err)
	}

	if info.Running {
		return 0, errors.New("exec output ended while the command was still running")
	}

	return info.ExitCode, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker daemon: %w", err)
	}

	return resp, nil
}

// apiError turns an error response into an error carrying the daemon's own
// message ({"message": "..."}), e.g. "No such container: db" or "Container
// ... is not running".
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var msg struct {
		Message string `json:"message"`
	}

	if json.Unmarshal(b, &msg) == nil && msg.Message != "" {
		return fmt.Errorf("docker daemon: %s", msg.Message)
	}

	return fmt.Errorf("docker daemon: %s: %s", resp.Status, strings.TrimSpace(string(b)))
}
