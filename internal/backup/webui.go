package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
// of store's job/target statuses (see dashboardHTML and handleStatus),
// reporting it (and any later failure) to stderr. Binding happens
// synchronously so a bad address is reported immediately and callers can
// rely on the server being reachable as soon as this returns; it returns nil
// if addr couldn't be bound, leaving the web UI disabled for this run rather
// than failing the whole process over a dashboard.
func startWebUI(addr string, store *statusStore, stderr io.Writer) *webUIServer {
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error: web UI: listening on", addr, "-", err)
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleDashboard)
	mux.HandleFunc("GET /api/status", handleStatus(store))

	srv := &webUIServer{
		http: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second},
		done: make(chan struct{}),
		addr: ln.Addr().String(),
	}

	go func() {
		defer close(srv.done)

		if err := srv.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintln(stderr, "error: web UI:", err)
		}
	}()

	_, _ = fmt.Fprintf(stderr, "web UI listening on %s\n", srv.addr)

	return srv
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
</style>
</head>
<body>
<h1>go-backup-tool</h1>
<p class="sub" id="updated">loading&hellip;</p>
<div class="grid" id="jobs"></div>

<script>
function fmtTime(s) {
  if (!s) return "never";
  const d = new Date(s);
  // The Go zero time.Time serializes as "0001-01-01T00:00:00Z" for a job
  // that has never run, rather than an empty/omitted field.
  if (d.getUTCFullYear() <= 1) return "never";
  return d.toLocaleString();
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

    return '<div class="card">' +
      '<div class="card-head"><span class="job-name">' + j.name + '</span>' + badge(j.state) + '</div>' +
      '<div class="meta">' + interval + ' &middot; last run: ' + fmtTime(j.last_start) + duration + '</div>' +
      err +
      '<ul class="targets">' + targets + '</ul>' +
      '</div>';
  }).join("");
}

function refresh() {
  fetch("/api/status")
    .then(function (r) { return r.json(); })
    .then(function (jobs) {
      render(jobs);
      document.getElementById("updated").textContent = "updated " + new Date().toLocaleTimeString();
    })
    .catch(function (err) {
      document.getElementById("updated").textContent = "error fetching status: " + err;
    });
}

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
