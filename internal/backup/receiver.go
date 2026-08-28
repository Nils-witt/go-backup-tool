package backup

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// fileReceiver is one top-level receivers: entry, defining a path this
// instance accepts incoming objects into over its receiver API (see
// webui.go's handleReceiveObject/handleDeleteObject), for pairing with
// another go-backup-tool instance's type: remote target. Only reachable
// when listen: is set, since the receiver API is served by the same HTTP
// server as the web UI dashboard.
type fileReceiver struct {
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
	StaleAfter string      `yaml:"stale-after"` // optional, same duration syntax as retention: e.g. "6h" or "1d"; requires webhook.url: and enables the stale-receiver webhook monitor (see monitorStaleReceivers)
	Webhook    fileWebhook `yaml:"webhook"`     // the request sent once this receiver's most recent file turns older than stale-after: (a receiver that has never received anything never fires); webhook.url requires stale-after:
}

// fileWebhook is a receiver's webhook: block: the HTTP request the
// stale-receiver monitor sends (see monitorStaleReceivers/
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

// resolvedWebhook is one fileWebhook after validation, ready to be sent by
// notifyStaleReceiverWebhook. A zero value (url == "") means the receiver
// it belongs to has no webhook: configured.
type resolvedWebhook struct {
	url     string
	method  string            // resolved: defaults to http.MethodPost when unset in the config file
	headers map[string]string // may be nil; Content-Type falls back to defaultStaleWebhookContentType if not among these
	body    string            // "" means the default JSON body (see staleReceiverPayload)
}

// resolvedReceiver is one fileReceiver after validation, ready to be used by
// the receiver API's handlers.
type resolvedReceiver struct {
	id         string
	publicKey  *rsa.PublicKey
	path       string
	retention  time.Duration
	staleAfter time.Duration   // 0 disables the stale-receiver webhook monitor for this receiver
	webhook    resolvedWebhook // set together with staleAfter; zero value means unset
}

// buildReceivers validates fileReceivers and builds an id -> resolvedReceiver
// map, requiring every entry to have a unique, non-empty id, a valid RSA
// public-key:, and a non-empty path.
func buildReceivers(fileReceivers []fileReceiver) (map[string]resolvedReceiver, error) {
	receivers := make(map[string]resolvedReceiver, len(fileReceivers))

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

		receivers[id] = resolvedReceiver{
			id:         id,
			publicKey:  publicKey,
			path:       fr.Path,
			retention:  retention,
			staleAfter: staleAfter,
			webhook:    webhook,
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
// http.MethodPost; headers, if any, are copied so the resolvedWebhook
// doesn't alias the config file's own map. A receiver with no webhook: at
// all (fw's zero value) resolves to a zero resolvedWebhook.
func buildWebhook(fw *fileWebhook, staleAfter time.Duration) (resolvedWebhook, error) {
	url := strings.TrimSpace(fw.URL)

	if (staleAfter > 0) != (url != "") {
		return resolvedWebhook{}, errors.New("stale-after and webhook.url must be set together")
	}

	if url == "" {
		if fw.Method != "" || len(fw.Headers) > 0 || fw.Body != "" {
			return resolvedWebhook{}, errors.New("webhook.method/headers/body require webhook.url")
		}

		return resolvedWebhook{}, nil
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

	return resolvedWebhook{url: url, method: method, headers: headers, body: fw.Body}, nil
}

// parseStaleAfter parses a receiver's stale-after: string into a
// time.Duration, using the same "d for days" syntax as retention:
// (parseDayDuration). An empty string means the stale-receiver webhook
// monitor is disabled for this receiver (the zero value); anything else must
// be positive, since "stale after zero (or a negative) time" is always true
// and so isn't a meaningful setting.
func parseStaleAfter(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	d, err := parseDayDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing stale-after %q: %w", s, err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("stale-after must be positive, got %q", s)
	}

	return d, nil
}

// sanitizeObjectKey validates key (the {key...} wildcard segment of a
// receiver API request path) before it's joined into a filesystem path.
// Although the request is already authenticated, key still comes from the
// network and must not be trusted to stay within its receiver's root: this
// rejects an empty/absolute key or one containing a "." or ".." path
// segment.
func sanitizeObjectKey(key string) (string, error) {
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

// receiverTarget builds a synthetic local target for recv, so the receiver
// API's handlers can reuse writeLocalObject/recordLocalWrite/
// deleteLocalObject/removeRetentionRecord (see pipeline.go and retention.go)
// exactly as a type: local target does — a receiver is a local target
// written to over HTTP with JWT auth, not a distinct storage
// implementation.
func receiverTarget(recv resolvedReceiver) *target {
	return &target{
		serverName: "receiver:" + recv.id,
		kind:       serverKindLocal,
		localPath:  recv.path,
		retention:  recv.retention,
	}
}

// receiverFile is one object currently stored under a receiver's path, as
// reported over /api/receivers/{id}/files, for display in the web UI
// dashboard (see webui.go).
type receiverFile struct {
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// listReceiverFiles walks recv.path and returns every object currently
// stored there, keyed the same way handleReceiveObject/handleDeleteObject
// address them (the path relative to recv.path, using "/" separators
// regardless of OS), sorted by creation time ascending (oldest first). A root
// directory that doesn't exist yet (nothing has been received) is not an
// error — it just yields no files.
// Temp files left behind by an interrupted write (see writeLocalObject's
// ".*.tmp" pattern) are skipped since they're not yet complete objects.
func listReceiverFiles(recv resolvedReceiver) ([]receiverFile, error) {
	if _, err := os.Stat(recv.path); err != nil {
		if os.IsNotExist(err) {
			return []receiverFile{}, nil
		}

		return nil, err
	}

	files := []receiverFile{}

	err := filepath.WalkDir(recv.path, func(path string, d fs.DirEntry, walkErr error) error {
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

		rel, err := filepath.Rel(recv.path, path)
		if err != nil {
			return err
		}

		files = append(files, receiverFile{Key: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime()})

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.Before(files[j].ModTime) })

	return files, nil
}

// lastReceivedAt returns the most recent modification time among every
// object currently stored under recv.path, skipping in-progress temp files
// the same way listReceiverFiles does — used by the stale-receiver webhook
// monitor (see monitorStaleReceivers) as the "last received" signal, since it
// reflects what's actually on disk regardless of process restarts (unlike
// receiverStatusStore's in-memory LastSeen). ok is false if recv.path
// doesn't exist yet or holds no objects, meaning nothing has ever been
// received.
func lastReceivedAt(recv resolvedReceiver) (t time.Time, ok bool, err error) {
	if _, statErr := os.Stat(recv.path); statErr != nil {
		if os.IsNotExist(statErr) {
			return time.Time{}, false, nil
		}

		return time.Time{}, false, statErr
	}

	walkErr := filepath.WalkDir(recv.path, func(_ string, d fs.DirEntry, err error) error {
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

// receiverSnapshot is one configured receiver's current status, as reported
// over /api/receivers, for display in the web UI dashboard (see webui.go).
// StaleAfter and Stale are filled in by handleReceiverStatus (see
// annotateReceiverStaleness), not newReceiverStatusStore/record below, since
// they reflect what's actually on disk (like lastReceivedAt) rather than
// live state this process tracks as requests come in.
type receiverSnapshot struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Retention  string    `json:"retention,omitempty"`
	State      runState  `json:"state"` // idle until the first object is received
	LastKey    string    `json:"last_key,omitempty"`
	LastSeen   time.Time `json:"last_seen"` // zero if nothing has been received yet
	Error      string    `json:"error,omitempty"`
	StaleAfter string    `json:"stale_after,omitempty"` // set only when this receiver has stale-after: configured
	Stale      bool      `json:"stale,omitempty"`       // true once the most recent file is older than StaleAfter; always false when StaleAfter is unset
}

// receiverStatusStore tracks the live state of every configured receiver,
// updated as the receiver API's handlers (see handleReceiveObject/
// handleDeleteObject in webui.go) serve incoming requests, and read by the
// web UI's HTTP handlers. Safe for concurrent use, since receiver requests
// can arrive concurrently with each other and with status requests.
type receiverStatusStore struct {
	mu    sync.Mutex
	byID  map[string]*receiverSnapshot
	order []string // receiver ids, sorted, for stable UI listing
}

// newReceiverStatusStore builds a receiverStatusStore with one idle entry
// per entry in receivers, listed in id order (receivers is keyed by id, so
// no other ordering survives buildReceivers).
func newReceiverStatusStore(receivers map[string]resolvedReceiver) *receiverStatusStore {
	ids := make([]string, 0, len(receivers))
	for id := range receivers {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	s := &receiverStatusStore{byID: make(map[string]*receiverSnapshot, len(receivers)), order: ids}

	for _, id := range ids {
		recv := receivers[id]
		s.byID[id] = &receiverSnapshot{ID: id, Path: recv.path, Retention: intervalString(recv.retention), State: stateIdle}
	}

	return s
}

// record updates receiver id's status after it just handled a request for
// key: err nil marks it ok, non-nil marks it failed with err's message,
// mirroring statusStore.targetDone's success/failure recording for job
// targets.
func (s *receiverStatusStore) record(id, key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]
	if !ok {
		return
	}

	r.LastKey = key
	r.LastSeen = time.Now()

	if err != nil {
		r.State = stateFailed
		r.Error = err.Error()

		return
	}

	r.State = stateOK
	r.Error = ""
}

// snapshot returns every receiver's current status, in id order, each a copy
// safe to serialize without holding s.mu.
func (s *receiverStatusStore) snapshot() []receiverSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]receiverSnapshot, 0, len(s.order))

	for _, id := range s.order {
		out = append(out, *s.byID[id])
	}

	return out
}

// recordReceiverEventBestEffort appends a receiver_events row for one
// handleReceiveObject/handleDeleteObject request, win or lose, so the daily
// report (see report.go) can summarize receiver activity later. A nil db
// (the state db couldn't be opened at startup) is a no-op, matching every
// other state-db write in this package; a write failure is logged rather
// than returned, since it shouldn't affect the response the request already
// committed to.
func recordReceiverEventBestEffort(ctx context.Context, db *sql.DB, log *slog.Logger, receiverID, kind, key string, size int64, recvErr error) {
	if db == nil {
		return
	}

	ev := receiverEvent{At: time.Now(), ReceiverID: receiverID, Kind: kind, Key: key, Size: size, Success: recvErr == nil}
	if recvErr != nil {
		ev.Error = recvErr.Error()
	}

	if err := recordReceiverEvent(ctx, db, ev); err != nil {
		log.Warn("receiver: recording event failed", "id", receiverID, "err", err)
	}
}

// sweepStartupReceiverRetention runs one retention sweep, before the web UI
// starts serving, for every receiver with retention: set — mirroring
// sweepStartupRetention's reasoning for local server targets: without this,
// a receiver would only get swept whenever it next happens to receive a
// write, potentially long after files there actually expired. A nil db
// (retention tracking unavailable this run) is a no-op.
func sweepStartupReceiverRetention(ctx context.Context, db *sql.DB, receivers map[string]resolvedReceiver, log *slog.Logger) {
	if db == nil {
		return
	}

	for _, recv := range receivers {
		if recv.retention <= 0 {
			continue
		}

		t := receiverTarget(recv)

		log.Debug("startup receiver retention sweep", "id", recv.id, "path", recv.path, "retention", recv.retention)

		if err := sweepRetentionForTarget(ctx, db, t, log); err != nil {
			log.Warn("startup receiver retention sweep failed", "id", recv.id, "err", err)
		}
	}
}

// staleReceiverCheckInterval is how often monitorStaleReceivers re-checks
// every receiver with stale-after: set.
const staleReceiverCheckInterval = time.Minute

// staleWebhookTimeout bounds a single stale-receiver webhook POST, since
// notifyStaleReceiverWebhook runs on its own background schedule rather than
// under a run's -timeout.
const staleWebhookTimeout = 10 * time.Second

// staleReceiverMonitor tracks, per receiver id, whether its stale webhook has
// already fired for the receiver's current gap in incoming files, so
// monitorStaleReceivers fires it once per gap instead of on every check —
// the gap clears (and the webhook can fire again) once a fresh file arrives
// and brings the receiver back under its stale-after: threshold.
type staleReceiverMonitor struct {
	mu       sync.Mutex
	notified map[string]bool
}

// newStaleReceiverMonitor returns a staleReceiverMonitor with no receiver yet
// marked as notified.
func newStaleReceiverMonitor() *staleReceiverMonitor {
	return &staleReceiverMonitor{notified: make(map[string]bool)}
}

// check evaluates recv's current staleness — nothing received within
// recv.staleAfter — and fires its webhook exactly once per gap. It never
// fires for a receiver that has never received anything at all: with no
// file on disk there's nothing to be stale, so that's left to whatever
// alerting already watches for a receiver that should have received its
// first file by now. A recv.staleAfter <= 0 (the monitor disabled for this
// receiver) is also a no-op.
func (m *staleReceiverMonitor) check(recv resolvedReceiver, log *slog.Logger) {
	if recv.staleAfter <= 0 {
		return
	}

	lastSeen, ok, err := lastReceivedAt(recv)
	if err != nil {
		log.Warn("stale receiver check: listing files failed", "id", recv.id, "err", err)
		return
	}

	stale := ok && time.Since(lastSeen) > recv.staleAfter

	m.mu.Lock()
	alreadyNotified := m.notified[recv.id]
	m.notified[recv.id] = stale
	m.mu.Unlock()

	if !stale || alreadyNotified {
		return
	}

	notifyStaleReceiverWebhook(recv, lastSeen, log)
}

// staleReceiverPayload is the default JSON body POSTed to a receiver's
// webhook:, used unless the receiver's webhook-payload: overrides it (see
// renderStaleWebhookPayload).
type staleReceiverPayload struct {
	ReceiverID   string    `json:"receiver_id"`
	Path         string    `json:"path"`
	StaleAfter   string    `json:"stale_after"`
	LastReceived time.Time `json:"last_received"`
}

// defaultStaleWebhookContentType is the Content-Type sent with a stale
// receiver webhook request whose receiver doesn't set a Content-Type among
// webhook.headers:.
const defaultStaleWebhookContentType = "application/json"

// staleWebhookBody builds the request body sent to recv.webhook: recv's
// webhook.body: template (see renderStaleWebhookPayload), if set, otherwise
// the default JSON staleReceiverPayload.
func staleWebhookBody(recv resolvedReceiver, lastSeen time.Time) ([]byte, error) {
	if recv.webhook.body != "" {
		return []byte(renderStaleWebhookPayload(recv.webhook.body, recv, lastSeen)), nil
	}

	payload := staleReceiverPayload{
		ReceiverID: recv.id, Path: recv.path, StaleAfter: recv.staleAfter.String(), LastReceived: lastSeen,
	}

	return json.Marshal(payload)
}

// renderStaleWebhookPayload substitutes a receiver's webhook.body: template's
// placeholders with recv's current staleness, mirroring how a job's key:
// substitutes {time} (see substituteKeyTime): {receiver_id}, {path}, and
// {stale_after} are recv's own fields; {last_received} is lastSeen formatted
// as RFC 3339. This lets an operator's webhook receiver (Slack, PagerDuty, a
// custom endpoint expecting its own JSON/form shape, ...) get a body it
// already understands instead of go-backup-tool's own default shape.
func renderStaleWebhookPayload(tmpl string, recv resolvedReceiver, lastSeen time.Time) string {
	replacer := strings.NewReplacer(
		"{receiver_id}", recv.id,
		"{path}", recv.path,
		"{stale_after}", recv.staleAfter.String(),
		"{last_received}", lastSeen.UTC().Format(time.RFC3339),
	)

	return replacer.Replace(tmpl)
}

// notifyStaleReceiverWebhook sends recv's current staleness to recv.webhook,
// logging (rather than returning) any failure: a webhook delivery problem
// shouldn't affect anything else this process is doing, and there's no
// caller to report it to — monitorStaleReceivers already marked this gap as
// notified before calling this, so a failed delivery isn't retried until the
// gap clears and reopens.
func notifyStaleReceiverWebhook(recv resolvedReceiver, lastSeen time.Time, log *slog.Logger) {
	body, err := staleWebhookBody(recv, lastSeen)
	if err != nil {
		log.Warn("stale receiver webhook: encoding payload failed", "id", recv.id, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), staleWebhookTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, recv.webhook.method, recv.webhook.url, bytes.NewReader(body))
	if err != nil {
		log.Warn("stale receiver webhook: building request failed", "id", recv.id, "err", err)
		return
	}

	for k, v := range recv.webhook.headers {
		req.Header.Set(k, v)
	}

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", defaultStaleWebhookContentType)
	}

	resp, err := staleWebhookHTTPClient.Do(req)
	if err != nil {
		log.Warn("stale receiver webhook: request failed", "id", recv.id, "webhook", recv.webhook.url, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("stale receiver webhook: non-2xx response", "id", recv.id, "webhook", recv.webhook.url, "status", resp.StatusCode)
		return
	}

	log.Info("stale receiver webhook fired", "id", recv.id, "webhook", recv.webhook.url, "stale_after", recv.staleAfter, "last_received", lastSeen)
}

// staleWebhookHTTPClient is shared by every stale-receiver webhook POST;
// staleWebhookTimeout bounds each request since these run on a background
// schedule rather than under a run's -timeout.
var staleWebhookHTTPClient = &http.Client{Timeout: staleWebhookTimeout}

// monitorStaleReceivers periodically checks every receiver with stale-after:
// set, POSTing to its webhook: whenever the most recent file under its path
// (see lastReceivedAt) is older than stale-after. A receiver that has never
// received anything at all never fires — there's no file to be stale — so
// this only alerts on a sender that stopped showing up, not one that never
// started. It checks once immediately, then every staleReceiverCheckInterval,
// until ctx is done; a receiver's webhook fires once per gap (see
// staleReceiverMonitor), not on every check, so a sender that stays down
// doesn't spam the webhook indefinitely. A no-op if no receiver has
// stale-after: set.
func monitorStaleReceivers(ctx context.Context, receivers map[string]resolvedReceiver, log *slog.Logger) {
	if !anyReceiverHasStaleAfter(receivers) {
		return
	}

	monitor := newStaleReceiverMonitor()

	checkAll := func() {
		for _, recv := range receivers {
			monitor.check(recv, log)
		}
	}

	checkAll()

	ticker := time.NewTicker(staleReceiverCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAll()
		}
	}
}

// anyReceiverHasStaleAfter reports whether any entry in receivers has
// stale-after: set, i.e. whether monitorStaleReceivers has anything to do.
func anyReceiverHasStaleAfter(receivers map[string]resolvedReceiver) bool {
	for _, recv := range receivers {
		if recv.staleAfter > 0 {
			return true
		}
	}

	return false
}
