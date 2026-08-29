// The Go zero time.Time serializes as "0001-01-01T00:00:00Z" rather than an
// empty/omitted field.
function hasTime(s) {
  return !!s && new Date(s).getUTCFullYear() > 1;
}

function fmtTime(s) {
  if (!hasTime(s)) return "never";
  return new Date(s).toLocaleString();
}

function badge(state, label) {
  return '<span class="badge ' + state + '">' + (label || state) + '</span>';
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

// expandedReceivers/receiverFilesCache/lastReceivers hold client-only UI
// state across refresh() polls: which receiver cards have their file list
// open, the last-fetched file list per receiver id, and the last /api/receivers
// payload (so a files fetch completing asynchronously can re-render without
// waiting on the next poll).
let expandedReceivers = {};
let receiverFilesCache = {};
let lastReceivers = [];

function fmtSize(bytes) {
  if (bytes < 1024) return bytes + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let val = bytes;
  let i = -1;
  do {
    val /= 1024;
    i++;
  } while (val >= 1024 && i < units.length - 1);
  return val.toFixed(1) + " " + units[i];
}

// escapeHtml escapes s for safe inclusion as HTML text. File keys, unlike
// the rest of this dashboard's data, originate from the receiver API's
// caller (any holder of a receiver's bearer token) rather than this
// instance's own config, so they're escaped before going into innerHTML.
function escapeHtml(s) {
  return s.replace(/[&<>"']/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
  });
}

// encodePathKey percent-encodes each segment of a "/"-separated file key on
// its own, so the download link's URL keeps the key's directory structure
// (matching the server's {key...} wildcard route) while still escaping
// anything else that needs it.
function encodePathKey(key) {
  return key.split("/").map(encodeURIComponent).join("/");
}

function renderFileList(id) {
  const files = receiverFilesCache[id];
  if (!files) return '<p class="meta">loading&hellip;</p>';
  if (!files.length) return '<p class="meta">no files stored</p>';

  return '<ul class="files">' + files.map(function (f) {
    const href = "/api/receivers/" + encodeURIComponent(id) + "/download/" + encodePathKey(f.key);
    return '<li><span class="file-key">' + escapeHtml(f.key) + '</span>' +
      '<span class="file-meta">' + fmtSize(f.size) + ' &middot; ' + fmtTime(f.mod_time) +
      ' &middot; <a href="' + href + '" class="download-link" data-key="' + escapeHtml(f.key) + '">download</a></span></li>';
  }).join("") + '</ul>';
}

function toggleReceiverFiles(id) {
  if (expandedReceivers[id]) {
    expandedReceivers[id] = false;
    renderReceivers(lastReceivers);
    return;
  }

  expandedReceivers[id] = true;
  delete receiverFilesCache[id];
  renderReceivers(lastReceivers);

  fetch("/api/receivers/" + encodeURIComponent(id) + "/files")
    .then(function (r) { return r.json(); })
    .then(function (files) {
      receiverFilesCache[id] = files;
      renderReceivers(lastReceivers);
    })
    .catch(function () {
      receiverFilesCache[id] = [];
      renderReceivers(lastReceivers);
    });
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
    const staleAfter = rcv.stale_after ? (' &middot; stale after ' + rcv.stale_after) : '';
    const staleBadge = rcv.stale ? badge('failed', 'stale') : '';
    const lastSeen = hasTime(rcv.last_seen)
      ? ('last received: ' + fmtTime(rcv.last_seen) + (rcv.last_key ? ' (' + rcv.last_key + ')' : ''))
      : 'no objects received yet';
    const expanded = !!expandedReceivers[rcv.id];
    const filesSection = expanded ? '<div class="files-wrap">' + renderFileList(rcv.id) + '</div>' : '';

    return '<div class="card">' +
      '<div class="card-head"><span class="job-name">' + rcv.id + '</span>' +
        '<span class="badges">' + badge(rcv.state) + staleBadge + '</span>' +
      '</div>' +
      '<div class="meta">' + rcv.path + retention + staleAfter + '</div>' +
      '<div class="meta">' + lastSeen + '</div>' +
      err +
      '<button type="button" class="files-toggle" data-id="' + rcv.id + '">' +
        (expanded ? "Hide files" : "Show files") +
      '</button>' +
      filesSection +
      '</div>';
  }).join("");
}

// download confirmation dialog: clicking a download link opens the native
// <dialog> in dashboard.html instead of navigating immediately; the actual
// navigation happens on "close" only if the form's submitted value is
// "confirm" (the Download button), so Cancel and Esc both fall through as
// a no-op.
const downloadDialog = document.getElementById("download-confirm-dialog");
const downloadDialogKey = document.getElementById("download-confirm-key");
let pendingDownloadHref = null;

downloadDialog.addEventListener("close", function () {
  if (downloadDialog.returnValue === "confirm" && pendingDownloadHref) {
    window.location.href = pendingDownloadHref;
  }
  pendingDownloadHref = null;
});

document.getElementById("receivers").addEventListener("click", function (e) {
  const link = e.target.closest(".download-link");
  if (link) {
    e.preventDefault();
    pendingDownloadHref = link.getAttribute("href");
    downloadDialogKey.textContent = link.dataset.key;
    downloadDialog.showModal();
    return;
  }

  const btn = e.target.closest(".files-toggle");
  if (!btn) return;
  toggleReceiverFiles(btn.dataset.id);
});

// renderLogs re-renders the log viewer from lines (oldest first, as served
// by /api/logs). It preserves the reader's scroll position across refreshes
// unless "Follow" is checked and they're already at (or near) the bottom, so
// a poll landing mid-read doesn't yank the view down.
function renderLogs(lines) {
  const wrap = document.getElementById("logs-wrap");
  if (!lines || !lines.length) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  const pre = document.getElementById("logs");
  const follow = document.getElementById("follow-logs").checked;
  const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 4;

  pre.innerHTML = lines.map(function (line) {
    let cls = "";
    if (line.indexOf("level=ERROR") !== -1) cls = "log-error";
    else if (line.indexOf("level=WARN") !== -1) cls = "log-warn";
    return '<span class="' + cls + '">' + escapeHtml(line) + '</span>';
  }).join("\n");

  if (follow && atBottom) {
    pre.scrollTop = pre.scrollHeight;
  }
}

// renderLoginEvents re-renders the login log table from events (newest
// first, as served by /api/login-events). The section stays hidden while
// there's nothing to show, matching the receivers/logs sections above.
function renderLoginEvents(events) {
  const wrap = document.getElementById("login-log-wrap");
  if (!events || !events.length) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  document.getElementById("login-log-body").innerHTML = events.map(function (ev) {
    const result = ev.success ? badge("ok", "success") : badge("failed", "failed");
    const detail = ev.detail ? '<p class="err">' + escapeHtml(ev.detail) + '</p>' : '';
    return '<tr>' +
      '<td class="nowrap">' + fmtTime(ev.at) + '</td>' +
      '<td>' + escapeHtml(ev.username || '(unknown)') + '</td>' +
      '<td class="nowrap">' + escapeHtml(ev.method) + '</td>' +
      '<td class="nowrap">' + result + detail + '</td>' +
      '<td>' + escapeHtml(ev.remote_addr) + '</td>' +
      '</tr>';
  }).join("");
}

// renderDownloadEvents re-renders the download log table from events
// (newest first, as served by /api/download-events). The section stays
// hidden while there's nothing to show, matching renderLoginEvents above.
function renderDownloadEvents(events) {
  const wrap = document.getElementById("download-log-wrap");
  if (!events || !events.length) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  document.getElementById("download-log-body").innerHTML = events.map(function (ev) {
    const result = ev.success ? badge("ok", "success") : badge("failed", "failed");
    const detail = ev.detail ? '<p class="err">' + escapeHtml(ev.detail) + '</p>' : '';
    return '<tr>' +
      '<td class="nowrap">' + fmtTime(ev.at) + '</td>' +
      '<td>' + escapeHtml(ev.username || '(unknown)') + '</td>' +
      '<td class="nowrap">' + escapeHtml(ev.receiver_id) + '</td>' +
      '<td>' + escapeHtml(ev.key) + '</td>' +
      '<td class="nowrap">' + result + detail + '</td>' +
      '<td>' + escapeHtml(ev.remote_addr) + '</td>' +
      '</tr>';
  }).join("");
}

// renderIdentity fills in the "Server identity" section from identity (as
// served by /api/identity). It stays hidden when there's no identity to
// show, matching the receivers/logs sections above — either
// loadServerIdentityAtStartup failed at startup, or the response hasn't
// loaded yet.
function renderIdentity(identity) {
  const wrap = document.getElementById("identity-wrap");
  if (!identity || !identity.uuid) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  document.getElementById("identity-uuid").textContent = identity.uuid;
  document.getElementById("identity-pubkey").textContent = identity.public_key;
}

// identityLoaded tracks whether /api/identity has been fetched yet: unlike
// the rest of the dashboard, this data never changes while the process is
// running, so it's fetched once rather than on every refresh() poll.
let identityLoaded = false;

function loadIdentity() {
  fetch("/api/identity")
    .then(function (r) { return r.json(); })
    .then(function (identity) {
      identityLoaded = true;
      renderIdentity(identity);
    })
    .catch(function () {});
}

function refresh() {
  if (!identityLoaded) loadIdentity();

  Promise.all([
    fetch("/api/status").then(function (r) { return r.json(); }),
    fetch("/api/receivers").then(function (r) { return r.json(); }),
    fetch("/api/logs").then(function (r) { return r.json(); }),
    fetch("/api/login-events").then(function (r) { return r.json(); }),
    fetch("/api/download-events").then(function (r) { return r.json(); })
  ]).then(function (results) {
    render(results[0]);
    lastReceivers = results[1] || [];
    renderReceivers(lastReceivers);
    renderLogs(results[2]);
    renderLoginEvents(results[3]);
    renderDownloadEvents(results[4]);
    document.getElementById("updated").textContent = "updated " + new Date().toLocaleTimeString();
  }).catch(function (err) {
    document.getElementById("updated").textContent = "error fetching status: " + err;
  });
}

refresh();
setInterval(refresh, 2000);
