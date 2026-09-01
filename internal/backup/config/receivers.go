package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
)

// FileReceiver is one top-level receivers: entry, defining a path this
// instance accepts incoming objects into over its receiver API (see
// webui.go's handleReceiveObject/handleDeleteObject), for pairing with
// another go-backup-tool instance's type: remote target. Only reachable
// when listen: is set, since the receiver API is served by the same HTTP
// server as the web UI dashboard.
type FileReceiver struct {
	ID string `yaml:"id"`

	// PublicKey is the PEM-encoded RSA public key of the sending instance
	// allowed to write to this receiver — the contents of that instance's
	// own generated data/keys/server.pub (see ensureServerKeyPair). Every
	// request must present a JSON Web Token signed with the matching
	// private key (see signRemoteAuthToken/verifyRemoteAuthToken and
	// authorizeReceiver in webui.go); unlike the token: field this replaces,
	// nothing here is itself a secret; it just names who's allowed to send.
	PublicKey  string      `yaml:"public-key"`
	Path       string      `yaml:"path"`        // root directory incoming objects for this id are written under
	Retention  string      `yaml:"retention"`   // optional, same syntax as a local server's retention: e.g. "30d"
	StaleAfter string      `yaml:"stale-after"` // optional, same duration syntax as retention: e.g. "6h" or "1d"; requires webhook.url: and enables the stale-receiver webhook monitor (see MonitorStaleReceivers)
	Webhook    fileWebhook `yaml:"webhook"`     // the request sent once this receiver's most recent file turns older than stale-after: (a receiver that has never received anything never fires); webhook.url requires stale-after:
}

// fileWebhook is a receiver's webhook: block: the HTTP request the
// stale-receiver monitor sends (see MonitorStaleReceivers/
// notifyStaleReceiverWebhook) once that receiver goes stale. Only url is
// required; method defaults to POST, headers is optional (e.g.
// Content-Type), and body, if unset, defaults to a JSON summary (see
// staleReceiverPayload) — set it to send a body your own webhook receiver
// (PagerDuty, Slack, ...) already understands, using the {placeholder}
// syntax documented on renderStaleWebhookPayload.
type fileWebhook struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

// ResolvedWebhook is one fileWebhook after validation, ready to be sent by
// the receiver package's stale-receiver monitor. A zero value (URL == "")
// means the receiver it belongs to has no webhook: configured.
type ResolvedWebhook struct {
	URL     string
	Method  string            // resolved: defaults to http.MethodPost when unset in the config file
	Headers map[string]string // may be nil; Content-Type falls back to a default if not among these
	Body    string            // "" means the default JSON body
}

// ResolvedReceiver is one fileReceiver after validation, ready to be used by
// the receiver API's handlers.
type ResolvedReceiver struct {
	ID         string
	PublicKey  *rsa.PublicKey
	Path       string
	Retention  time.Duration
	StaleAfter time.Duration   // 0 disables the stale-receiver webhook monitor for this receiver
	Webhook    ResolvedWebhook // set together with StaleAfter; zero value means unset
}

// buildReceivers validates fileReceivers and builds an id -> resolvedReceiver
// map, requiring every entry to have a unique, non-empty id, a valid RSA
// public-key:, and a non-empty path.
func buildReceivers(fileReceivers []FileReceiver) (map[string]ResolvedReceiver, error) {
	receivers := make(map[string]ResolvedReceiver, len(fileReceivers))

	for i, fr := range fileReceivers {
		id := strings.TrimSpace(fr.ID)
		if id == "" {
			return nil, fmt.Errorf("receivers[%d]: id is required", i)
		}

		if _, exists := receivers[id]; exists {
			return nil, fmt.Errorf("receivers[%d]: duplicate receiver id %q", i, id)
		}

		publicKey, err := parseReceiverPublicKey(fr.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		if strings.TrimSpace(fr.Path) == "" {
			return nil, fmt.Errorf("receiver %q: path is required", id)
		}

		retention, err := parseRetention(fr.Retention)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		staleAfter, err := parseStaleAfter(fr.StaleAfter)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		webhook, err := buildWebhook(&fr.Webhook, staleAfter)
		if err != nil {
			return nil, fmt.Errorf("receiver %q: %w", id, err)
		}

		receivers[id] = ResolvedReceiver{
			ID:         id,
			PublicKey:  publicKey,
			Path:       fr.Path,
			Retention:  retention,
			StaleAfter: staleAfter,
			Webhook:    webhook,
		}
	}

	return receivers, nil
}

// parseReceiverPublicKey parses raw (a receiver's public-key: value) as a
// PEM-encoded PKIX public key — the same format ensureServerKeyPair writes
// to server.pub — requiring it to be an RSA key, since that's the only
// algorithm signRemoteAuthToken/verifyRemoteAuthToken sign and verify with.
func parseReceiverPublicKey(raw string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("public-key is required and must be a PEM-encoded PUBLIC KEY block")
	}

	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing public-key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public-key must be an RSA key, got %T", key)
	}

	return rsaKey, nil
}

// buildWebhook validates fw (a receiver's webhook: block) against that
// receiver's already-parsed staleAfter, and resolves it: url must be set
// together with staleAfter (both, or neither); method defaults to
// http.MethodPost; headers, if any, are copied so the ResolvedWebhook
// doesn't alias the config file's own map. A receiver with no webhook: at
// all (fw's zero value) resolves to a zero ResolvedWebhook.
func buildWebhook(fw *fileWebhook, staleAfter time.Duration) (ResolvedWebhook, error) {
	url := strings.TrimSpace(fw.URL)

	if (staleAfter > 0) != (url != "") {
		return ResolvedWebhook{}, errors.New("stale-after and webhook.url must be set together")
	}

	if url == "" {
		if fw.Method != "" || len(fw.Headers) > 0 || fw.Body != "" {
			return ResolvedWebhook{}, errors.New("webhook.method/headers/body require webhook.url")
		}

		return ResolvedWebhook{}, nil
	}

	method := strings.ToUpper(strings.TrimSpace(fw.Method))
	if method == "" {
		method = http.MethodPost
	}

	var headers map[string]string
	if len(fw.Headers) > 0 {
		headers = make(map[string]string, len(fw.Headers))
		maps.Copy(headers, fw.Headers)
	}

	return ResolvedWebhook{URL: url, Method: method, Headers: headers, Body: fw.Body}, nil
}

// parseStaleAfter parses a receiver's stale-after: string into a
// time.Duration, using the same "d for days" syntax as retention:
// (parseDayDuration). An empty string means the stale-receiver webhook
// monitor is disabled for this receiver (the zero value); anything else must
// be positive, since "stale after zero (or a negative) time" is always true
// and so isn't a meaningful setting.
func parseStaleAfter(s string) (time.Duration, error) {
	return parseOptionalDayDuration("stale-after", s, false)
}
