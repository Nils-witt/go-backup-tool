package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
)

// maxResponseBytes caps how much of a response body client reads, so a
// misbehaving server can't exhaust memory.
const maxResponseBytes = 16 << 20

// errNoInstanceURL is resolveBaseURL's error when nothing names the
// instance.
var errNoInstanceURL = errors.New("no instance URL: pass -url, set " + urlEnv + ", or enable the web UI (webui.enabled, webui.listen) in the config file")

// resolveBaseURL returns the running instance's base URL: opts.url, else
// $GO_BACKUP_TOOL_URL, else one derived from the config file's
// webui.listen (see listenURL).
func resolveBaseURL(opts options) (*url.URL, error) {
	raw := opts.url
	if raw == "" {
		raw = os.Getenv(urlEnv)
	}

	if raw == "" {
		listen, err := config.WebUIListen(opts.configPath, opts.configExplicit)
		if err != nil {
			return nil, fmt.Errorf("finding the instance from the config file: %w", err)
		}

		if listen == "" {
			return nil, errNoInstanceURL
		}

		return listenURL(listen)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing instance URL: %w", err)
	}

	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("instance URL %q must be an http:// or https:// URL", raw)
	}

	return u, nil
}

// listenURL turns a webui.listen address into the URL to reach it on this
// host: a wildcard or empty host (":8080", "0.0.0.0:8080", "[::]:8080")
// becomes localhost. The web UI serves only plain HTTP itself, so the
// scheme is always http.
func listenURL(listen string) (*url.URL, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("parsing webui.listen %q: %w", listen, err)
	}

	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}

	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}, nil
}

// client calls a running instance's JSON API.
type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// newClient returns a client for the instance at base, sending token (else
// $GO_BACKUP_TOOL_TOKEN, if set) as a bearer token.
func newClient(base *url.URL, token string, timeout time.Duration) *client {
	if token == "" {
		token = os.Getenv(tokenEnv)
	}

	return &client{base: base, token: strings.TrimSpace(token), http: &http.Client{Timeout: timeout}}
}

// statusError is a non-2xx response.
type statusError struct {
	path string
	code int
	body string
}

func (e *statusError) Error() string {
	msg := fmt.Sprintf("GET %s: %d %s", e.path, e.code, http.StatusText(e.code))

	switch e.code {
	case http.StatusUnauthorized:
		msg += " (set " + tokenEnv + " to an API token from the dashboard's Settings › API tokens)"
	case http.StatusForbidden:
		msg += " (the API token lacks the permission this needs)"
	default:
		if e.body != "" {
			msg += ": " + e.body
		}
	}

	return msg
}

// get fetches path, returning the status code and body of any response
// (2xx or not); the error is only for a request that got no response.
func (c *client) get(ctx context.Context, path string) (int, []byte, error) {
	target := c.base.JoinPath(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("reaching instance: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("reading %s response: %w", path, err)
	}

	return resp.StatusCode, body, nil
}

// getJSON fetches path and decodes its 2xx JSON body into v; any other
// status is a *statusError.
func (c *client) getJSON(ctx context.Context, path string, v any) error {
	code, body, err := c.get(ctx, path)
	if err != nil {
		return err
	}

	if code < 200 || code > 299 {
		return &statusError{path: path, code: code, body: strings.TrimSpace(sanitize(string(body)))}
	}

	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decoding %s response: %w", path, err)
	}

	return nil
}
