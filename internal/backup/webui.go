package backup

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// webUIServer wraps the HTTP server behind the -listen web UI, letting
// callers shut it down cleanly (see startWebUI/shutdown).
type webUIServer struct {
	http *http.Server
	done chan struct{}
	addr string // the listener's actual bound address, e.g. resolved from ":0"
}

// startWebUI binds addr and starts an HTTP server serving a live dashboard
// of store's job/target statuses (see dashboardHTML and handleStatus), plus
// the receiver API (see handleReceiveObject/handleDeleteObject) for any
// entries in receivers — whose own live status is tracked in a
// receiverStatusStore and served over /api/receivers (see
// handleReceiverStatus) — reporting it (and any later failure) to log.
// Binding happens synchronously so a bad address is reported immediately and
// callers can rely on the server being reachable as soon as this returns; it
// returns nil if addr couldn't be bound, leaving the web UI disabled for
// this run rather than failing the whole process over a dashboard. db is the
// shared state/retention db (see schedule_state.go and retention.go), used
// by the receiver API's handlers to track retention on incoming writes; nil
// disables that tracking (see recordLocalWrite).
func startWebUI(addr string, store *statusStore, receivers map[string]resolvedReceiver, log *slog.Logger, db *sql.DB) *webUIServer {
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		log.Error("web UI: listening", "addr", addr, "err", err)
		return nil
	}

	receiverStore := newReceiverStatusStore(receivers)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard)
	mux.HandleFunc("GET /api/status", handleStatus(store))
	mux.HandleFunc("GET /api/receivers", handleReceiverStatus(receiverStore))
	mux.HandleFunc("PUT /api/v1/objects/{id}/{key...}", handleReceiveObject(receivers, receiverStore, log, db))
	mux.HandleFunc("DELETE /api/v1/objects/{id}/{key...}", handleDeleteObject(receivers, receiverStore, log, db))

	srv := &webUIServer{
		http: &http.Server{Handler: logRequests(log, mux), ReadHeaderTimeout: 10 * time.Second},
		done: make(chan struct{}),
		addr: ln.Addr().String(),
	}

	go func() {
		defer close(srv.done)

		if err := srv.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("web UI", "err", err)
		}
	}()

	log.Info("web UI listening", "addr", srv.addr)

	return srv
}

// logRequests wraps next, logging every request it handles at debug level
// once it completes: method, path, remote address, response status, and
// how long it took — the same per-request detail an operator would reach
// for a proper HTTP access log, without pulling in a logging middleware
// dependency for a handful of routes.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, req)

		log.Debug("web UI request",
			"method", req.Method,
			"path", req.URL.Path,
			"remote", req.RemoteAddr,
			"status", sw.status,
			"duration", time.Since(start),
		)
	})
}

// statusWriter wraps an http.ResponseWriter to capture the status code
// written to it, since http.ResponseWriter doesn't expose that itself once
// WriteHeader has been called.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// shutdown gracefully stops the web UI server, waiting for it to finish.
func (s *webUIServer) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = s.http.Shutdown(ctx)

	<-s.done
}

// handleStatus serves store's current job/target statuses as JSON.
func handleStatus(store *statusStore) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(store.snapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// handleReceiverStatus serves store's current receiver statuses as JSON.
func handleReceiverStatus(store *receiverStatusStore) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		if err := json.NewEncoder(w).Encode(store.snapshot()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// handleReceiveObject serves PUT /api/v1/objects/{id}/{key...}: after
// authorizing the request against receivers (see authorizeReceiver), it
// writes the request body to disk exactly as a type: local target would
// (see receiverTarget in receiver.go), so a remote target's PUT and this
// instance's own local-target writes share the same on-disk behavior
// (atomic temp-file-then-rename) and retention tracking. Every attempt is
// recorded to status, win or lose, so /api/receivers reflects it.
func handleReceiveObject(receivers map[string]resolvedReceiver, status *receiverStatusStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := authorizeReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := sanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cfg := &config{key: key, stateDB: db}
		t := receiverTarget(recv)

		if err := writeLocalObject(cfg, t, r.Body); err != nil {
			log.Warn("receiver: writing object failed", "id", recv.id, "key", key, "err", err)
			status.record(recv.id, key, err)
			http.Error(w, "writing object failed", http.StatusInternalServerError)

			return
		}

		if err := recordLocalWrite(r.Context(), cfg, t, log); err != nil {
			log.Warn("receiver: retention tracking failed", "id", recv.id, "key", key, "err", err)
		}

		status.record(recv.id, key, nil)
		log.Info("receiver: object written", "id", recv.id, "key", key, "path", localObjectPath(cfg, t))
		w.WriteHeader(http.StatusCreated)
	}
}

// handleDeleteObject serves DELETE /api/v1/objects/{id}/{key...}, the
// receiver API's client-facing counterpart to deleteRemoteObject in
// pipeline.go. Every attempt is recorded to status, win or lose, so
// /api/receivers reflects it.
func handleDeleteObject(receivers map[string]resolvedReceiver, status *receiverStatusStore, log *slog.Logger, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recv, ok := authorizeReceiver(w, r, receivers)
		if !ok {
			return
		}

		key, err := sanitizeObjectKey(r.PathValue("key"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cfg := &config{key: key, stateDB: db}
		t := receiverTarget(recv)

		if err := deleteLocalObject(cfg, t); err != nil {
			log.Warn("receiver: deleting object failed", "id", recv.id, "key", key, "err", err)
			status.record(recv.id, key, err)
			http.Error(w, "deleting object failed", http.StatusInternalServerError)

			return
		}

		if err := removeRetentionRecord(r.Context(), cfg, t); err != nil {
			log.Warn("receiver: removing retention record failed", "id", recv.id, "key", key, "err", err)
		}

		status.record(recv.id, key, nil)
		log.Info("receiver: object deleted", "id", recv.id, "key", key, "path", localObjectPath(cfg, t))
		w.WriteHeader(http.StatusNoContent)
	}
}

// authorizeReceiver looks up the receiver named by the request's {id} path
// value and checks its Authorization: Bearer <token> header, writing an
// error response and returning ok=false if either the id is unknown or the
// token doesn't match.
func authorizeReceiver(w http.ResponseWriter, r *http.Request, receivers map[string]resolvedReceiver) (recv resolvedReceiver, ok bool) {
	recv, exists := receivers[r.PathValue("id")]
	if !exists {
		http.Error(w, "unknown receiver id", http.StatusNotFound)
		return resolvedReceiver{}, false
	}

	token, hasToken := bearerToken(r)
	// subtle.ConstantTimeCompare, rather than ==, so a mismatch can't be
	// timed to learn how many leading bytes of the token were guessed
	// correctly.
	if !hasToken || subtle.ConstantTimeCompare([]byte(token), []byte(recv.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return resolvedReceiver{}, false
	}

	return recv, true
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// request header, reporting false if the header is missing or malformed.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}

	return strings.TrimPrefix(auth, prefix), true
}

// handleDashboard serves the static dashboard page, which polls
// /api/status itself; the page has no server-rendered state.
func handleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

// dashboardHTML is the entire web UI: a single self-contained page (no
// external assets) that polls /api/status every couple of seconds and
// re-renders the job/target table.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>go-backup-tool</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f7f7f8;
    --fg: #1c1c1e;
    --muted: #6b6b70;
    --card: #ffffff;
    --border: #e2e2e5;
    --ok: #1a7f37;
    --ok-bg: #e7f6ec;
    --failed: #b3261e;
    --failed-bg: #fdeceb;
    --running: #9a6700;
    --running-bg: #fff6dc;
    --incomplete: #bf5b04;
    --incomplete-bg: #fff0e0;
    --idle: #6b6b70;
    --idle-bg: #eeeef0;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #17171a;
      --fg: #eaeaec;
      --muted: #9a9aa0;
      --card: #202024;
      --border: #313136;
      --ok: #56d364;
      --ok-bg: #123321;
      --failed: #ff7b72;
      --failed-bg: #3a1414;
      --running: #e3b341;
      --running-bg: #3a2e0a;
      --incomplete: #ffa657;
      --incomplete-bg: #3a2410;
      --idle: #9a9aa0;
      --idle-bg: #2a2a2e;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 2rem 1.5rem 4rem;
    background: var(--bg);
    color: var(--fg);
    font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  h1 {
    font-size: 1.25rem;
    margin: 0 0 .25rem;
  }
  .sub {
    color: var(--muted);
    font-size: .85rem;
    margin: 0 0 1.5rem;
  }
  .grid {
    display: grid;
    gap: 1rem;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    max-width: 1200px;
    margin: 0 auto;
  }
  .card {
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 1rem 1.1rem;
  }
  .card-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: .5rem;
    margin-bottom: .5rem;
  }
  .job-name {
    font-weight: 600;
    font-size: 1rem;
  }
  .meta {
    color: var(--muted);
    font-size: .8rem;
    margin-bottom: .6rem;
  }
  .badge {
    display: inline-block;
    font-size: .72rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: .02em;
    padding: .15rem .5rem;
    border-radius: 999px;
    white-space: nowrap;
  }
  .badge.ok { color: var(--ok); background: var(--ok-bg); }
  .badge.failed { color: var(--failed); background: var(--failed-bg); }
  .badge.running { color: var(--running); background: var(--running-bg); }
  .badge.incomplete { color: var(--incomplete); background: var(--incomplete-bg); }
  .badge.idle { color: var(--idle); background: var(--idle-bg); }
  .targets {
    list-style: none;
    margin: 0;
    padding: 0;
    border-top: 1px solid var(--border);
  }
  .targets li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: .5rem;
    padding: .45rem 0;
    border-bottom: 1px solid var(--border);
    font-size: .85rem;
  }
  .target-name {
    overflow-wrap: anywhere;
  }
  .target-name .kind {
    color: var(--muted);
    font-size: .75rem;
  }
  .err {
    margin: .3rem 0 0;
    font-size: .78rem;
    color: var(--failed);
    overflow-wrap: anywhere;
  }
  .empty {
    color: var(--muted);
    text-align: center;
    margin-top: 3rem;
  }
  .section-title {
    font-size: 1.05rem;
    margin: 2.5rem auto 1rem;
    max-width: 1200px;
  }
</style>
</head>
<body>
<h1>go-backup-tool</h1>
<p class="sub" id="updated">loading&hellip;</p>
<div class="grid" id="jobs"></div>

<div id="receivers-wrap" hidden>
  <h2 class="section-title">Receivers</h2>
  <div class="grid" id="receivers"></div>
</div>

<script>
// The Go zero time.Time serializes as "0001-01-01T00:00:00Z" rather than an
// empty/omitted field.
function hasTime(s) {
  return !!s && new Date(s).getUTCFullYear() > 1;
}

function fmtTime(s) {
  if (!hasTime(s)) return "never";
  return new Date(s).toLocaleString();
}

function badge(state) {
  return '<span class="badge ' + state + '">' + state + '</span>';
}

function render(jobs) {
  const grid = document.getElementById("jobs");
  if (!jobs.length) {
    grid.innerHTML = '<p class="empty">no jobs configured</p>';
    return;
  }

  grid.innerHTML = jobs.map(function (j) {
    const targets = (j.targets || []).map(function (t) {
      const err = t.error ? '<p class="err">' + t.error + '</p>' : '';
      return '<li><span class="target-name">' + t.server + ' / ' + t.bucket +
        ' <span class="kind">(' + t.kind + ')</span>' + err + '</span>' + badge(t.state) + '</li>';
    }).join("");

    const err = j.error ? '<p class="err">' + j.error + '</p>' : '';
    const interval = j.interval ? ('every ' + j.interval) : 'runs once';
    const duration = j.duration ? (' &middot; took ' + j.duration) : '';
    const size = j.size ? (' &middot; ' + j.size) : '';
    const nextRun = hasTime(j.next_run) ? (' &middot; next run: ' + fmtTime(j.next_run)) : '';

    return '<div class="card">' +
      '<div class="card-head"><span class="job-name">' + j.name + '</span>' + badge(j.state) + '</div>' +
      '<div class="meta">' + interval + ' &middot; last run: ' + fmtTime(j.last_start) + duration + size + nextRun + '</div>' +
      err +
      '<ul class="targets">' + targets + '</ul>' +
      '</div>';
  }).join("");
}

function renderReceivers(receivers) {
  const wrap = document.getElementById("receivers-wrap");
  if (!receivers.length) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  document.getElementById("receivers").innerHTML = receivers.map(function (rcv) {
    const err = rcv.error ? '<p class="err">' + rcv.error + '</p>' : '';
    const retention = rcv.retention ? (' &middot; retention ' + rcv.retention) : '';
    const lastSeen = hasTime(rcv.last_seen)
      ? ('last received: ' + fmtTime(rcv.last_seen) + (rcv.last_key ? ' (' + rcv.last_key + ')' : ''))
      : 'no objects received yet';

    return '<div class="card">' +
      '<div class="card-head"><span class="job-name">' + rcv.id + '</span>' + badge(rcv.state) + '</div>' +
      '<div class="meta">' + rcv.path + retention + '</div>' +
      '<div class="meta">' + lastSeen + '</div>' +
      err +
      '</div>';
  }).join("");
}

function refresh() {
  Promise.all([
    fetch("/api/status").then(function (r) { return r.json(); }),
    fetch("/api/receivers").then(function (r) { return r.json(); })
  ]).then(function (results) {
    render(results[0]);
    renderReceivers(results[1]);
    document.getElementById("updated").textContent = "updated " + new Date().toLocaleTimeString();
  }).catch(function (err) {
    document.getElementById("updated").textContent = "error fetching status: " + err;
  });
}

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
