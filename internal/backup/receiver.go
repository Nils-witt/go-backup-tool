package backup

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// SanitizeObjectKey validates key (the {key...} wildcard segment of a
// receiver API request path) before it's joined into a filesystem path.
// Although the request is already authenticated, key still comes from the
// network and must not be trusted to stay within its receiver's root: this
// rejects an empty/absolute key or one containing a "." or ".." path
// segment.
func SanitizeObjectKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("invalid key %q", key)
	}

	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid key %q", key)
		}
	}

	return key, nil
}

// ReceiverTarget builds a synthetic local target for recv, so the receiver
// API's handlers can reuse WriteLocalObject/RecordLocalWrite/
// DeleteLocalObject/RemoveRetentionRecord (see pipeline.go and retention.go)
// exactly as a type: local target does — a receiver is a local target
// written to over HTTP with JWT auth, not a distinct storage
// implementation.
func ReceiverTarget(recv ResolvedReceiver) *Target {
	return &Target{
		ServerName: "receiver:" + recv.ID,
		Kind:       ServerKindLocal,
		LocalPath:  recv.Path,
		Retention:  recv.Retention,
	}
}

// ReceiverFile is one object currently stored under a receiver's path, as
// reported over /api/receivers/{id}/files, for display in the web UI
// dashboard (see webui.go).
type ReceiverFile struct {
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// ListReceiverFiles walks recv.Path and returns every object currently
// stored there, keyed the same way handleReceiveObject/handleDeleteObject
// address them (the path relative to recv.Path, using "/" separators
// regardless of OS), sorted by creation time ascending (oldest first). A root
// directory that doesn't exist yet (nothing has been received) is not an
// error — it just yields no files.
// Temp files left behind by an interrupted write (see WriteLocalObject's
// ".*.tmp" pattern) are skipped since they're not yet complete objects.
func ListReceiverFiles(recv ResolvedReceiver) ([]ReceiverFile, error) {
	if _, err := os.Stat(recv.Path); err != nil {
		if os.IsNotExist(err) {
			return []ReceiverFile{}, nil
		}

		return nil, err
	}

	files := []ReceiverFile{}

	err := filepath.WalkDir(recv.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(recv.Path, path)
		if err != nil {
			return err
		}

		files = append(files, ReceiverFile{Key: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime()})

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.Before(files[j].ModTime) })

	return files, nil
}

// LastReceivedAt returns the most recent modification time among every
// object currently stored under recv.Path, skipping in-progress temp files
// the same way ListReceiverFiles does — used by the stale-receiver webhook
// monitor (see MonitorStaleReceivers) as the "last received" signal, since it
// reflects what's actually on disk regardless of process restarts (unlike
// receiverStatusStore's in-memory LastSeen). ok is false if recv.Path
// doesn't exist yet or holds no objects, meaning nothing has ever been
// received.
func LastReceivedAt(recv ResolvedReceiver) (t time.Time, ok bool, err error) {
	if _, statErr := os.Stat(recv.Path); statErr != nil {
		if os.IsNotExist(statErr) {
			return time.Time{}, false, nil
		}

		return time.Time{}, false, statErr
	}

	walkErr := filepath.WalkDir(recv.Path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.ModTime().After(t) {
			t = info.ModTime()
			ok = true
		}

		return nil
	})
	if walkErr != nil {
		return time.Time{}, false, walkErr
	}

	return t, ok, nil
}

// ReceiverSnapshot is one configured receiver's current status, as reported
// over /api/receivers, for display in the web UI dashboard (see webui.go).
// StaleAfter and Stale are filled in by handleReceiverStatus (see
// annotateReceiverStaleness), not NewReceiverStatusStore/record below, since
// they reflect what's actually on disk (like LastReceivedAt) rather than
// live state this process tracks as requests come in.
type ReceiverSnapshot struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Retention  string    `json:"retention,omitempty"`
	State      RunState  `json:"state"` // idle until the first object is received
	LastKey    string    `json:"last_key,omitempty"`
	LastSeen   time.Time `json:"last_seen"` // zero if nothing has been received yet
	Error      string    `json:"error,omitempty"`
	StaleAfter string    `json:"stale_after,omitempty"` // set only when this receiver has stale-after: configured
	Stale      bool      `json:"stale,omitempty"`       // true once the most recent file is older than StaleAfter; always false when StaleAfter is unset
}

// ReceiverStatusStore tracks the live state of every configured receiver,
// updated as the receiver API's handlers (see handleReceiveObject/
// handleDeleteObject in webui.go) serve incoming requests, and read by the
// web UI's HTTP handlers. Safe for concurrent use, since receiver requests
// can arrive concurrently with each other and with status requests.
type ReceiverStatusStore struct {
	mu    sync.Mutex
	byID  map[string]*ReceiverSnapshot
	order []string // receiver ids, sorted, for stable UI listing
}

// NewReceiverStatusStore builds a receiverStatusStore with one idle entry
// per entry in receivers, listed in id order (receivers is keyed by id, so
// no other ordering survives buildReceivers).
func NewReceiverStatusStore(receivers map[string]ResolvedReceiver) *ReceiverStatusStore {
	ids := make([]string, 0, len(receivers))
	for id := range receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	s := &ReceiverStatusStore{byID: make(map[string]*ReceiverSnapshot, len(receivers)), order: ids}

	for _, id := range ids {
		recv := receivers[id]
		s.byID[id] = &ReceiverSnapshot{ID: id, Path: recv.Path, Retention: intervalString(recv.Retention), State: StateIdle}
	}

	return s
}

// Record updates receiver id's status after it just handled a request for
// key: err nil marks it ok, non-nil marks it failed with err's message,
// mirroring statusStore.targetDone's success/failure recording for job
// targets.
func (s *ReceiverStatusStore) Record(id, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]
	if !ok {
		return
	}

	r.LastKey = key
	r.LastSeen = time.Now()

	SetOutcome(&r.State, &r.Error, err)
}

// SeedLastEvent initializes receiver id's snapshot from ev, its most
// recently persisted receiver_events row (see readLastReceiverEvent in
// schedule_state.go), mirroring statusStore.seedLastRun's reasoning for job
// status: a restart's web UI can still show a receiver's last activity
// instead of it reverting to idle until it next serves a request. Called
// once at startup, before the receiver API can serve any request, so it
// never races an actual record call.
func (s *ReceiverStatusStore) SeedLastEvent(id string, ev ReceiverEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]
	if !ok {
		return
	}

	r.LastKey = ev.Key
	r.LastSeen = ev.At

	if ev.Success {
		r.State = StateOK
		r.Error = ""

		return
	}

	r.State = StateFailed
	r.Error = ev.Error
}

// Snapshot returns every receiver's current status, in id order, each a copy
// safe to serialize without holding s.mu.
func (s *ReceiverStatusStore) Snapshot() []ReceiverSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ReceiverSnapshot, 0, len(s.order))

	for _, id := range s.order {
		out = append(out, *s.byID[id])
	}

	return out
}
