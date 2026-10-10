package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// auditBodyPeek bounds how much of a create request's body recordChange
// reads ahead of the handler to find the new item's id (see
// auditTargetFromBody). A larger body is still passed on to the handler
// intact; its target just goes unrecorded.
const auditBodyPeek = 1 << 20

// auditDetailLimit bounds how much of a failed change's error response
// recordChange keeps as the audit event's detail.
const auditDetailLimit = 512

// recordChange wraps next — a handler that changes this instance's stored
// configuration, already behind requirePermission — appending one audit
// event per request to db's audit log (see store.Store.SaveAuditEvent),
// win or lose: who made it, what it targeted (see describeChange), and the
// response status, plus the error text on failure. For a successful change
// to a collection in resources, it also records each field the change
// made, with its old and new value (see diffSnapshots), from a snapshot of
// the item taken before and after next ran. The request body itself is
// never recorded, since it may carry secrets. A write failure there is
// only logged — a change must never be blocked by an audit-log hiccup. A
// nil db records nothing, matching StartWebUI's optional db.
func recordChange(db *store.Store, resources map[string]auditResource, log *slog.Logger, trustProxyHeaders bool, next http.HandlerFunc) http.HandlerFunc {
	if db == nil {
		return next
	}

	return func(w http.ResponseWriter, r *http.Request) {
		action, resource, target := describeChange(r)
		if target == "" && r.Method == http.MethodPost {
			target = auditTargetFromBody(r)
		}

		res, tracked := resources[resource]

		var before any

		// A create of a named item starts from nothing: anything already
		// stored under that name (e.g. an older API token sharing it) isn't
		// what the create changed. A create with no target (a GPG key
		// import) changes a collection that does already exist.
		if tracked && (action != "create" || target == "") {
			var err error

			if before, _, err = res.snapshot(r.Context(), target); err != nil {
				log.Warn("web UI: audit snapshot failed", "resource", resource, "target", target, "err", err)

				tracked = false
			}
		}

		aw := &auditWriter{ResponseWriter: w, status: http.StatusOK}

		next(aw, r)

		username := ""
		if user, ok := currentUser(r.Context()); ok {
			username = user.Username
		}

		success := aw.status < http.StatusBadRequest

		detail := ""
		if !success {
			detail = strings.TrimSpace(aw.body.String())
		}

		var changes []store.AuditChange
		if tracked && success {
			changes = auditChanges(context.WithoutCancel(r.Context()), log, res, resource, target, before)
		}

		ev := store.AuditEvent{
			At: time.Now(), Username: username, Action: action, Resource: resource, Target: target,
			Method: r.Method, Path: r.URL.Path, Status: aw.status, Success: success,
			RemoteAddr: clientAddr(r, trustProxyHeaders), Detail: detail, Changes: changes,
		}
		if err := db.SaveAuditEvent(context.WithoutCancel(r.Context()), ev); err != nil {
			log.Warn("web UI: recording audit event failed", "err", err)
		}
	}
}

// auditChanges snapshots res's target again, now that a change has been
// applied, and diffs it against before (see diffSnapshots). A failure only
// logs a warning and records no field changes.
func auditChanges(ctx context.Context, log *slog.Logger, res auditResource, resource, target string, before any) []store.AuditChange {
	after, _, err := res.snapshot(ctx, target)
	if err != nil {
		log.Warn("web UI: audit snapshot failed", "resource", resource, "target", target, "err", err)
		return nil
	}

	changes, err := diffSnapshots(before, after, res.secret)
	if err != nil {
		log.Warn("web UI: diffing audit snapshots failed", "resource", resource, "target", target, "err", err)
		return nil
	}

	return changes
}

// describeChange derives an audit event's action, resource, and target from
// the mux pattern r matched (e.g. "PUT /api/receiver-configs/{id}"): the
// resource is the first path segment after /api/, the target the first
// wildcard's value, and the action a literal segment following it (e.g.
// "retry" for POST /api/jobs/{name}/retry), falling back to the method's
// create/update/delete.
func describeChange(r *http.Request) (action, resource, target string) {
	_, path, _ := strings.Cut(r.Pattern, " ")
	segs := strings.Split(strings.TrimPrefix(path, "/api/"), "/")
	resource = segs[0]

	for _, seg := range segs[1:] {
		if name, ok := strings.CutPrefix(seg, "{"); ok {
			name = strings.TrimSuffix(strings.TrimSuffix(name, "}"), "...")
			if target == "" {
				target = r.PathValue(name)
			}

			continue
		}

		action = seg
	}

	if action == "" {
		switch r.Method {
		case http.MethodPost:
			action = "create"
		case http.MethodPut:
			action = "update"
		case http.MethodDelete:
			action = "delete"
		default:
			action = strings.ToLower(r.Method)
		}
	}

	return action, resource, target
}

// auditTargetFromBody returns the id of the item a create request's JSON
// body describes — its top-level "id", "name", or "key" string, whichever
// comes first — or "" if there's none or the body is too large to peek at
// (see auditBodyPeek). r.Body is replaced so the handler still reads it in
// full, through its own size limit.
func auditTargetFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}

	peek, err := io.ReadAll(io.LimitReader(r.Body, auditBodyPeek+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}

	if err != nil || len(peek) > auditBodyPeek {
		return ""
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(peek, &fields) != nil {
		return ""
	}

	for _, k := range []string{"id", "name", "key"} {
		var s string
		if json.Unmarshal(fields[k], &s) == nil && s != "" {
			return s
		}
	}

	return ""
}

// auditWriter wraps an http.ResponseWriter to capture the status code and,
// for an error response, the start of its body (see auditDetailLimit), for
// recordChange.
type auditWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *auditWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditWriter) Write(p []byte) (int, error) {
	if w.status >= http.StatusBadRequest && w.body.Len() < auditDetailLimit {
		w.body.Write(p[:min(len(p), auditDetailLimit-w.body.Len())])
	}

	return w.ResponseWriter.Write(p)
}

// Unwrap exposes the wrapped ResponseWriter, so http.MaxBytesReader and
// http.ResponseController can reach it through this wrapper.
func (w *auditWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// auditEventJSON is store.AuditEvent's wire shape for handleAuditEvents,
// matching the dashboard's own field naming (snake_case, as every other
// /api/... endpoint here uses).
type auditEventJSON struct {
	At         time.Time           `json:"at"`
	Username   string              `json:"username"`
	Action     string              `json:"action"`
	Resource   string              `json:"resource"`
	Target     string              `json:"target"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Status     int                 `json:"status"`
	Success    bool                `json:"success"`
	RemoteAddr string              `json:"remote_addr"`
	Detail     string              `json:"detail"`
	Changes    []store.AuditChange `json:"changes"`
}

// handleAuditEvents serves GET /api/audit-events: the most recently
// recorded configuration changes (see recordChange), newest first, as JSON.
// db nil (state tracking unavailable) serves an empty list rather than
// failing the request, matching handleLoginEvents's own tolerance for a
// missing dependency.
func handleAuditEvents(db *store.Store, log *slog.Logger, limit int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveEventLog(w, r, log, db, limit, db.ListAuditEvents,
			func(ev store.AuditEvent) auditEventJSON { return auditEventJSON(ev) },
			"reading audit events failed")
	}
}
