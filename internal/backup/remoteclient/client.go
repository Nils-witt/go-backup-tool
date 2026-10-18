// Package remoteclient is the client half of the receiver API: it uploads
// an object to another instance's receiver over HTTP(S), signing each
// request with this instance's identity (see the remoteauth package) and
// honoring the target's TLS settings. The server half is the receiver
// package.
package remoteclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/identity"
	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
)

// remoteDefaultClient is shared by every remote target's upload/delete
// requests that use Go's default TLS verification. It sets no client-side
// timeout: the request's context (derived from the run's overall -timeout,
// if any) governs cancellation instead, consistent with how local
// filesystem writes are bounded only by ctx.
var remoteDefaultClient = &http.Client{}

// remoteTLSClients caches one client per distinct config.TargetTLS (see
// remoteHTTPClient), so targets with the same settings share connections.
// Entries are never evicted: there is one per TLS setting ever used, which
// only changes when an admin edits a server.
var (
	remoteTLSClientsMu sync.Mutex
	remoteTLSClients   = map[config.TargetTLS]*http.Client{}
)

// remoteHTTPClient returns the client for t's TLS settings:
// remoteDefaultClient for the default verification, otherwise a cached
// client whose transport is http.DefaultTransport's settings plus t's
// tls.Config, and, like remoteDefaultClient, no client-side timeout.
func remoteHTTPClient(t *config.Target) (*http.Client, error) {
	if t.TLS.IsZero() {
		return remoteDefaultClient, nil
	}

	remoteTLSClientsMu.Lock()
	defer remoteTLSClientsMu.Unlock()

	if c, ok := remoteTLSClients[t.TLS]; ok {
		return c, nil
	}

	tlsCfg, err := t.TLS.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("server %q: %w", t.ServerName, err)
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}

	transport := base.Clone()
	transport.TLSClientConfig = tlsCfg

	c := &http.Client{Transport: transport}
	remoteTLSClients[t.TLS] = c

	return c, nil
}

// ObjectURL returns the URL a remote target (t.Kind ==
// config.ServerKindRemote) uploads key at: t.Endpoint's
// /api/v1/objects/{bucket}/{key} for a flat object, or — when file is set,
// one file of a multi-file backup — /api/v1/backups/{bucket}/{key} (the
// file itself goes in the query, see remoteObjectURL). t.Bucket is the id
// identifying the destination instance's receiver (see receiver.go); key is
// path-escaped since it may contain characters not otherwise safe in a URL
// path segment.
func ObjectURL(t *config.Target, key, file string) string {
	route := "objects"
	if file != "" {
		route = "backups"
	}

	return fmt.Sprintf("%s/api/v1/%s/%s/%s",
		strings.TrimSuffix(t.Endpoint, "/"), route, url.PathEscape(t.Bucket), url.PathEscape(key))
}

// remoteObjectURL is ObjectURL with its query string appended: file
// (when set) and query (e.g. Upload's creationTime), for
// doAuthenticatedRemoteRequest.
func remoteObjectURL(t *config.Target, key, file string, query url.Values) string {
	u := ObjectURL(t, key, file)

	if file != "" {
		query = maps.Clone(query)
		if query == nil {
			query = url.Values{}
		}

		query.Set("file", file)
	}

	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	return u
}

// remoteAuthHeader signs a fresh JWT with id (this instance's own RSA key
// and UUID — see identity.LoadServerIdentityAtStartup) scoped to t.Bucket
// (the destination instance's receiver id) and bound to req (see
// remoteauth.Request), and formats it as an Authorization: Bearer header
// value, for Upload. Errors if id is nil.
func remoteAuthHeader(id *identity.ServerIdentity, t *config.Target, req remoteauth.Request) (string, error) {
	if id == nil {
		return "", errors.New("no server identity available (see startup log for why loadServerIdentity failed)")
	}

	token, err := id.SignRequest(t.Bucket, req)
	if err != nil {
		return "", fmt.Errorf("signing request: %w", err)
	}

	return "Bearer " + token, nil
}

// Upload streams r as the body of a PUT request to another
// go-backup-tool instance's receiver API (target t, kind ==
// config.ServerKindRemote), authenticated with a JWT signed by id, this
// instance's own identity (see remoteAuthHeader). r is streamed directly as
// the request body, never buffered. When cfg.CreatedAt is set, it's sent as the
// creationTime query parameter (RFC3339), so the receiver bases retention on
// when this run's backup content was actually produced rather than on
// upload time — see receiver.parseCreationTime. A zero cfg.CreatedAt sends no
// creationTime at all, matching the receiver's own upload-time fallback.
// r must also implement io.Seeker (the staged *os.File does): it's read
// once up front to sign its SHA-256 into the request token (see
// remoteauth.Request), then rewound and streamed.
func Upload(ctx context.Context, id *identity.ServerIdentity, cfg *config.Config, t *config.Target, r io.Reader) error {
	var query url.Values
	if !cfg.CreatedAt.IsZero() {
		query = url.Values{"creationTime": {cfg.CreatedAt.UTC().Format(time.RFC3339)}}
	}

	return doAuthenticatedRemoteRequest(ctx, id, cfg, t, http.MethodPut, r, "application/octet-stream", query)
}

// FileUploadResult is a directory receiver's answer to UploadFile.
type FileUploadResult struct {
	// Stored is false when the path's newest version already had this
	// content, so the receiver kept nothing new.
	Stored bool `json:"stored"`

	// Version is the path's newest version number after the upload.
	Version int `json:"version"`
}

// maxFileUploadResponse bounds how much of a directory receiver's reply
// UploadFile reads.
const maxFileUploadResponse = 4096

// FileURL returns the URL an on-change job uploads relPath (a
// slash-separated path relative to its source) at on remote target t:
// t.Endpoint's /api/v1/files/{bucket}/{relPath}, each path segment escaped
// on its own so the receiver sees the same segments.
func FileURL(t *config.Target, relPath string) string {
	segments := strings.Split(relPath, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}

	return fmt.Sprintf("%s/api/v1/files/%s/%s",
		strings.TrimSuffix(t.Endpoint, "/"), url.PathEscape(t.Bucket), strings.Join(segments, "/"))
}

// UploadFile streams r, one encrypted file of an on-change job, to the
// directory receiver behind remote target t as relPath. plainSHA256 is the
// hex SHA-256 of the file before encryption: the receiver keeps a new
// version only when it differs from relPath's newest one. Both it and the
// body's own digest are bound into the request token (see
// remoteauth.Request); r must be seekable, as for Upload.
func UploadFile(ctx context.Context, id *identity.ServerIdentity, t *config.Target, relPath, plainSHA256 string, r io.Reader) (FileUploadResult, error) {
	bound := remoteauth.Request{Method: http.MethodPut, Key: relPath, PlainSHA256: plainSHA256}
	u := FileURL(t, relPath) + "?" + url.Values{"sha256": {plainSHA256}}.Encode()

	resp, err := sendSignedRequest(ctx, id, t, bound, u, r, "application/octet-stream")
	if err != nil {
		return FileUploadResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var res FileUploadResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFileUploadResponse)).Decode(&res); err != nil {
		return FileUploadResult{}, fmt.Errorf("reading receiver response: %w", err)
	}

	return res, nil
}

// doAuthenticatedRemoteRequest signs, sends, and status-checks a method
// request (with body, whose Content-Type is set only when contentType is
// non-empty, and an optional query string) to target t's receiver API, for
// Upload.
func doAuthenticatedRemoteRequest(ctx context.Context, id *identity.ServerIdentity, cfg *config.Config, t *config.Target, method string, body io.Reader, contentType string, query url.Values) error {
	bound := remoteauth.Request{Method: method, Key: cfg.Key, File: cfg.File, CreationTime: query.Get("creationTime")}

	resp, err := sendSignedRequest(ctx, id, t, bound, remoteObjectURL(t, cfg.Key, cfg.File, query), body, contentType)
	if err != nil {
		return err
	}

	return resp.Body.Close()
}

// sendSignedRequest sends a bound.Method request with body to u on target
// t's receiver API, authenticated with a token bound to bound plus body's
// SHA-256, and returns the response when its status is 2xx; the caller
// closes its body. Any other status is an error.
func sendSignedRequest(ctx context.Context, id *identity.ServerIdentity, t *config.Target, bound remoteauth.Request, u string, body io.Reader, contentType string) (*http.Response, error) {
	if body != nil {
		digest, err := bodySHA256(body)
		if err != nil {
			return nil, err
		}

		bound.BodySHA256 = digest
	}

	auth, err := remoteAuthHeader(id, t, bound)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, bound.Method, u, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", auth)
	setFileContentLength(req, body)

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client, err := remoteHTTPClient(t)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()

		return nil, remoteResponseError(resp)
	}

	return resp, nil
}

// bodySHA256 returns the hex SHA-256 of body's remaining content, then
// rewinds body to where it was, so it can still be streamed as the request
// body. body must implement io.Seeker; buffering an arbitrarily large
// backup in memory just to hash it isn't an option.
func bodySHA256(body io.Reader) (string, error) {
	seeker, ok := body.(io.ReadSeeker)
	if !ok {
		return "", errors.New("remote upload body must be seekable to sign its digest")
	}

	start, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", fmt.Errorf("finding body offset: %w", err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, seeker); err != nil {
		return "", fmt.Errorf("hashing body: %w", err)
	}

	if _, err := seeker.Seek(start, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewinding body: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// setFileContentLength sets req's ContentLength when body is a regular
// *os.File (the staged backup Upload streams), so the upload is
// sent with a Content-Length instead of chunked encoding, letting the
// receiving instance see the object's size up front. Still streamed from
// the file, never buffered. Any other body (or a Stat/Seek failure) keeps
// net/http's default of an unknown length.
func setFileContentLength(req *http.Request, body io.Reader) {
	f, ok := body.(*os.File)
	if !ok {
		return
	}

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}

	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	req.ContentLength = info.Size() - off
	if req.ContentLength == 0 {
		req.Body = http.NoBody
	}
}

// remoteResponseError builds an error from a non-2xx response, including a
// bounded read of the response body so a misbehaving or malicious server
// can't exhaust memory via an unbounded response.
func remoteResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	return fmt.Errorf("remote instance returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}
