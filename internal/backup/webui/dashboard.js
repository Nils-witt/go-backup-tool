// TOKEN_KEY is the sessionStorage key the dashboard's bearer token (see
// setToken/getToken) is kept under. sessionStorage, unlike localStorage, is
// cleared when the tab closes, so a closed-and-reopened tab requires a
// fresh login rather than staying signed in indefinitely.
const TOKEN_KEY = "gbt_webui_token";

function getToken() {
  try {
    return sessionStorage.getItem(TOKEN_KEY) || "";
  } catch (e) {
    return "";
  }
}

function setToken(token) {
  try {
    sessionStorage.setItem(TOKEN_KEY, token);
  } catch (e) {}
}

function clearToken() {
  try {
    sessionStorage.removeItem(TOKEN_KEY);
  } catch (e) {}
}

// goToLogin clears the stored token and sends the browser to the login
// page, remembering the current path so a successful login returns here.
function goToLogin() {
  clearToken();
  window.location.href = "/login?next=" + encodeURIComponent(window.location.pathname);
}

// apiFetch wraps fetch(), attaching the stored bearer token (if any) as an
// Authorization header on every call to the dashboard's own /api/...
// endpoints. A 401 response means the token is missing, invalid, or
// expired — there's no server-side redirect to fall back on (see
// requireWebUISession in webui.go), so this sends the browser to /login
// itself instead of letting the caller deal with an authentication failure.
function apiFetch(url, opts) {
  opts = opts || {};

  const headers = Object.assign({}, opts.headers || {});
  const token = getToken();
  if (token) headers["Authorization"] = "Bearer " + token;

  return fetch(url, Object.assign({}, opts, { headers: headers })).then(function (r) {
    if (r.status === 401) {
      goToLogin();
      throw new Error("unauthorized");
    }
    return r;
  });
}

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
    const downloadLink = canDownload()
      ? ' &middot; <a href="#" class="download-link" data-id="' + escapeHtml(id) + '" data-key="' + escapeHtml(f.key) + '">download</a>'
      : '';
    return '<li><span class="file-key">' + escapeHtml(f.key) + '</span>' +
      '<span class="file-meta">' + fmtSize(f.size) + ' &middot; ' + fmtTime(f.mod_time) +
      downloadLink + '</span></li>';
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

  apiFetch("/api/receivers/" + encodeURIComponent(id) + "/files")
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

// startDownload mints a one-time download ticket (see
// handleMintDownloadTicket in webui.go) with an authenticated request, then
// navigates the browser to the plain GET download URL with that ticket
// attached — a normal, browser-native download (no in-memory buffering of
// the whole file), authorized by the ticket rather than the Authorization
// header a navigation can't carry.
function startDownload(id, key) {
  const url = "/api/receivers/" + encodeURIComponent(id) + "/download/" + encodePathKey(key);

  apiFetch(url, { method: "POST" })
    .then(function (r) { return r.json(); })
    .then(function (data) {
      window.location.href = url + "?ticket=" + encodeURIComponent(data.ticket);
    })
    .catch(function () {});
}

// download confirmation dialog: clicking a download link opens the native
// <dialog> in dashboard.html instead of downloading immediately; the actual
// download happens on "close" only if the form's submitted value is
// "confirm" (the Download button), so Cancel and Esc both fall through as
// a no-op.
const downloadDialog = document.getElementById("download-confirm-dialog");
const downloadDialogKey = document.getElementById("download-confirm-key");
let pendingDownload = null;

downloadDialog.addEventListener("close", function () {
  if (downloadDialog.returnValue === "confirm" && pendingDownload) {
    startDownload(pendingDownload.id, pendingDownload.key);
  }
  pendingDownload = null;
});

document.getElementById("receivers").addEventListener("click", function (e) {
  const link = e.target.closest(".download-link");
  if (link) {
    e.preventDefault();
    pendingDownload = { id: link.dataset.id, key: link.dataset.key };
    downloadDialogKey.textContent = link.dataset.key;
    downloadDialog.showModal();
    return;
  }

  const btn = e.target.closest(".files-toggle");
  if (!btn) return;
  toggleReceiverFiles(btn.dataset.id);
});

// Log out: best-effort revoke the token server-side (see handleAPILogout in
// webui.go), then always clear it locally and send the browser to /login —
// there's no cookie or redirect for the server to clean up any more.
document.getElementById("logout-link").addEventListener("click", function (e) {
  e.preventDefault();

  const token = getToken();
  const headers = token ? { "Authorization": "Bearer " + token } : {};

  fetch("/api/logout", { method: "POST", headers: headers }).catch(function () {}).then(function () {
    clearToken();
    window.location.href = "/login";
  });
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
  apiFetch("/api/identity")
    .then(function (r) { return r.json(); })
    .then(function (identity) {
      identityLoaded = true;
      renderIdentity(identity);
    })
    .catch(function () {});
}

// sessionInfo holds the currently authenticated session's own permissions
// and admin status (as served by /api/session, see loadSessionInfo),
// starting as "no access" so a page that hasn't loaded it yet (or a
// request that failed) fails closed rather than briefly showing a
// download link or the "Users" section it turns out the session can't
// actually use.
let sessionInfo = { username: "", permissions: [], admin: false, oidc_enabled: false };
let sessionInfoLoaded = false;

function canDownload() {
  return sessionInfo.permissions.indexOf("download") !== -1;
}

function canViewLoginLog() {
  return sessionInfo.permissions.indexOf("login-log") !== -1;
}

function canViewDownloadLog() {
  return sessionInfo.permissions.indexOf("download-log") !== -1;
}

// sessionInfoLoaded tracks whether /api/session has been fetched yet,
// mirroring identityLoaded above: fetched once at startup rather than on
// every refresh() poll, since a session's own permissions don't change
// without logging in again.
function loadSessionInfo() {
  apiFetch("/api/session")
    .then(function (r) { return r.json(); })
    .then(function (info) {
      sessionInfoLoaded = true;
      sessionInfo = info || sessionInfo;
      renderReceivers(lastReceivers);
      renderUsersSection();
      renderOidcUsersSection();
      loadLoginEvents();
      loadDownloadEvents();
    })
    .catch(function () {});
}

// loadLoginEvents/loadDownloadEvents fetch and render the login/download
// history independently of refresh()'s main Promise.all — each is gated on
// its own permission (see canViewLoginLog/canViewDownloadLog) rather than
// PermissionView, so a session lacking one shouldn't have its 403 break the
// rest of the dashboard's refresh, and rendering an empty list for it looks
// the same as "no events yet" rather than surfacing an error.
function loadLoginEvents() {
  if (!canViewLoginLog()) {
    renderLoginEvents([]);
    return;
  }

  apiFetch("/api/login-events")
    .then(function (r) { return r.json(); })
    .then(function (events) { renderLoginEvents(events); })
    .catch(function () {});
}

function loadDownloadEvents() {
  if (!canViewDownloadLog()) {
    renderDownloadEvents([]);
    return;
  }

  apiFetch("/api/download-events")
    .then(function (r) { return r.json(); })
    .then(function (events) { renderDownloadEvents(events); })
    .catch(function () {});
}

// lastUsers holds the most recently fetched /api/users listing, so a
// permission checkbox's change handler and the remove-confirmation dialog
// don't have to re-fetch it themselves.
let lastUsers = [];

// renderUsersSection shows or hides the "Users" admin section based on
// sessionInfo.admin (see loadSessionInfo) — every dashboard viewer gets
// this markup, but only an admin session's own /api/users calls succeed,
// matching the server-side requireAdmin gate this mirrors client-side —
// and, when shown, (re-)loads its table.
function renderUsersSection() {
  const wrap = document.getElementById("users-wrap");
  if (!sessionInfoLoaded || !sessionInfo.admin) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  loadUsers();
}

function loadUsers() {
  apiFetch("/api/users")
    .then(function (r) { return r.json(); })
    .then(function (users) {
      lastUsers = users || [];
      renderUsersTable(lastUsers);
    })
    .catch(function () {});
}

function renderUsersTable(users) {
  document.getElementById("users-body").innerHTML = users.map(function (u) {
    const hasView = u.permissions.indexOf("view") !== -1;
    const hasDownload = u.permissions.indexOf("download") !== -1;
    const hasLoginLog = u.permissions.indexOf("login-log") !== -1;
    const hasDownloadLog = u.permissions.indexOf("download-log") !== -1;
    const hasAdmin = u.permissions.indexOf("admin") !== -1;
    const name = escapeHtml(u.username);

    return '<tr>' +
      '<td class="username">' + name + '</td>' +
      '<td><input type="checkbox" class="user-perm" data-username="' + name + '" data-perm="view"' + (hasView ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="user-perm" data-username="' + name + '" data-perm="download"' + (hasDownload ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="user-perm" data-username="' + name + '" data-perm="login-log"' + (hasLoginLog ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="user-perm" data-username="' + name + '" data-perm="download-log"' + (hasDownloadLog ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="user-perm" data-username="' + name + '" data-perm="admin"' + (hasAdmin ? ' checked' : '') + '></td>' +
      '<td>' + fmtTime(u.created_at) + '</td>' +
      '<td><a href="#" class="issue-token-link" data-username="' + name + '">issue token</a></td>' +
      '<td><a href="#" class="manage-tokens-link" data-username="' + name + '">tokens</a></td>' +
      '<td><a href="#" class="remove-user-link" data-username="' + name + '">remove</a></td>' +
      '</tr>';
  }).join("");
}

// A permission checkbox's change re-submits that row's whole permission
// set (not just the toggled box) as a PUT, since the API takes the full
// set rather than a single add/remove.
document.getElementById("users-body").addEventListener("change", function (e) {
  const box = e.target.closest(".user-perm");
  if (!box) return;

  const row = box.closest("tr");
  const perms = [];
  row.querySelectorAll(".user-perm").forEach(function (input) {
    if (input.checked) perms.push(input.dataset.perm);
  });

  apiFetch("/api/users/" + encodeURIComponent(box.dataset.username), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ permissions: perms })
  }).catch(function () {});
});

// Removing a user goes through the same confirm-dialog pattern as a
// download (see downloadDialog above).
const deleteUserDialog = document.getElementById("delete-user-confirm-dialog");
const deleteUserDialogUsername = document.getElementById("delete-user-confirm-username");
let pendingUserDelete = null;

deleteUserDialog.addEventListener("close", function () {
  if (deleteUserDialog.returnValue === "confirm" && pendingUserDelete) {
    apiFetch("/api/users/" + encodeURIComponent(pendingUserDelete), { method: "DELETE" })
      .then(function () { loadUsers(); })
      .catch(function () {});
  }
  pendingUserDelete = null;
});

document.getElementById("users-body").addEventListener("click", function (e) {
  const link = e.target.closest(".remove-user-link");
  if (!link) return;

  e.preventDefault();
  pendingUserDelete = link.dataset.username;
  deleteUserDialogUsername.textContent = link.dataset.username;
  deleteUserDialog.showModal();
});

// Issuing a long-lived API token for a "Users" admin-managed account: the
// issue-token-dialog collects how many days it should be valid for, then
// its close handler posts the request and, on success, shows the resulting
// token once in token-result-dialog (the server never shows it again).
const issueTokenDialog = document.getElementById("issue-token-dialog");
const issueTokenUsername = document.getElementById("issue-token-username");
const issueTokenDays = document.getElementById("issue-token-days");
let pendingTokenUser = null;

document.getElementById("users-body").addEventListener("click", function (e) {
  const link = e.target.closest(".issue-token-link");
  if (!link) return;

  e.preventDefault();
  pendingTokenUser = link.dataset.username;
  issueTokenUsername.textContent = link.dataset.username;
  issueTokenDays.value = "365";
  issueTokenDialog.showModal();
});

const tokenResultDialog = document.getElementById("token-result-dialog");
const tokenResultUsername = document.getElementById("token-result-username");
const tokenResultExpiry = document.getElementById("token-result-expiry");
const tokenResultValue = document.getElementById("token-result-value");
const tokenResultError = document.getElementById("token-result-error");

issueTokenDialog.addEventListener("close", function () {
  if (issueTokenDialog.returnValue !== "confirm" || !pendingTokenUser) {
    pendingTokenUser = null;
    return;
  }

  const username = pendingTokenUser;
  const days = parseInt(issueTokenDays.value, 10) || 365;
  pendingTokenUser = null;

  apiFetch("/api/users/" + encodeURIComponent(username) + "/tokens", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ days: days })
  }).then(function (r) {
    if (!r.ok) {
      return r.text().then(function (msg) {
        throw new Error(msg || "issuing token failed");
      });
    }
    return r.json();
  }).then(function (resp) {
    tokenResultUsername.textContent = username;
    tokenResultExpiry.textContent = fmtTime(resp.expires_at);
    tokenResultValue.value = resp.token;
    tokenResultError.hidden = true;
    tokenResultDialog.showModal();
  }).catch(function (err) {
    tokenResultUsername.textContent = username;
    tokenResultExpiry.textContent = "";
    tokenResultValue.value = "";
    tokenResultError.textContent = err.message || "issuing token failed";
    tokenResultError.hidden = false;
    tokenResultDialog.showModal();
  });
});

document.getElementById("token-result-copy").addEventListener("click", function () {
  tokenResultValue.select();
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(tokenResultValue.value).catch(function () {});
  } else {
    document.execCommand("copy");
  }
});

// Managing a user's already-issued long-lived API tokens: the
// tokens-dialog lists every token recorded for that user (see
// handleListWebUIUserTokens in webui.go) — the raw token value itself was
// only ever shown once, at issuance (see token-result-dialog above) — with
// a "revoke" link per still-active one that ends it early (see
// handleRevokeWebUIUserToken), without needing to hold the token itself.
const tokensDialog = document.getElementById("tokens-dialog");
const tokensDialogUsername = document.getElementById("tokens-dialog-username");
let tokensDialogUser = null;

function loadTokensDialog(username) {
  apiFetch("/api/users/" + encodeURIComponent(username) + "/tokens")
    .then(function (r) { return r.json(); })
    .then(function (tokens) { renderTokensDialog(tokens || []); })
    .catch(function () { renderTokensDialog([]); });
}

function renderTokensDialog(tokens) {
  document.getElementById("tokens-dialog-body").innerHTML = tokens.map(function (tok) {
    const status = tok.revoked ? badge("failed", "revoked") : badge("ok", "active");
    const revokeCell = tok.revoked ? "" :
      '<a href="#" class="revoke-token-link" data-jti="' + escapeHtml(tok.jti) + '">revoke</a>';

    return '<tr>' +
      '<td>' + fmtTime(tok.created_at) + '</td>' +
      '<td>' + fmtTime(tok.expires_at) + '</td>' +
      '<td>' + status + '</td>' +
      '<td>' + revokeCell + '</td>' +
      '</tr>';
  }).join("");
}

document.getElementById("users-body").addEventListener("click", function (e) {
  const link = e.target.closest(".manage-tokens-link");
  if (!link) return;

  e.preventDefault();
  tokensDialogUser = link.dataset.username;
  tokensDialogUsername.textContent = tokensDialogUser;
  renderTokensDialog([]);
  loadTokensDialog(tokensDialogUser);
  tokensDialog.showModal();
});

document.getElementById("tokens-dialog-body").addEventListener("click", function (e) {
  const link = e.target.closest(".revoke-token-link");
  if (!link || !tokensDialogUser) return;

  e.preventDefault();

  const username = tokensDialogUser;

  apiFetch("/api/users/" + encodeURIComponent(username) + "/tokens/" + encodeURIComponent(link.dataset.jti), {
    method: "DELETE"
  }).then(function () { loadTokensDialog(username); }).catch(function () {});
});

tokensDialog.addEventListener("close", function () {
  tokensDialogUser = null;
});

// Adding a user posts the form as JSON rather than letting the browser
// submit it as a normal form post, so the request can carry the
// Authorization header apiFetch attaches and the response's error (if any)
// can be shown inline instead of navigating away.
document.getElementById("add-user-form").addEventListener("submit", function (e) {
  e.preventDefault();

  const usernameInput = document.getElementById("add-user-username");
  const passwordInput = document.getElementById("add-user-password");
  const viewInput = document.getElementById("add-user-view");
  const downloadInput = document.getElementById("add-user-download");
  const loginLogInput = document.getElementById("add-user-login-log");
  const downloadLogInput = document.getElementById("add-user-download-log");
  const adminInput = document.getElementById("add-user-admin");
  const errEl = document.getElementById("add-user-error");

  const perms = [];
  if (viewInput.checked) perms.push("view");
  if (downloadInput.checked) perms.push("download");
  if (loginLogInput.checked) perms.push("login-log");
  if (downloadLogInput.checked) perms.push("download-log");
  if (adminInput.checked) perms.push("admin");

  errEl.hidden = true;

  apiFetch("/api/users", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: usernameInput.value.trim(), password: passwordInput.value, permissions: perms })
  }).then(function (r) {
    if (!r.ok) {
      return r.text().then(function (msg) {
        throw new Error(msg || "adding user failed");
      });
    }

    usernameInput.value = "";
    passwordInput.value = "";
    viewInput.checked = true;
    downloadInput.checked = false;
    loginLogInput.checked = false;
    downloadLogInput.checked = false;
    adminInput.checked = false;
    loadUsers();
  }).catch(function (err) {
    errEl.textContent = err.message || "adding user failed";
    errEl.hidden = false;
  });
});

// lastOidcUsers holds the most recently fetched /api/oidc-users listing,
// mirroring lastUsers above.
let lastOidcUsers = [];

// renderOidcUsersSection shows or hides the "OIDC users" admin section:
// like renderUsersSection, it's gated on sessionInfo.admin, and additionally
// on sessionInfo.oidc_enabled — there's nothing to override when SSO isn't
// configured at all.
function renderOidcUsersSection() {
  const wrap = document.getElementById("oidc-users-wrap");
  if (!sessionInfoLoaded || !sessionInfo.admin || !sessionInfo.oidc_enabled) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;

  loadOidcUsers();
}

function loadOidcUsers() {
  apiFetch("/api/oidc-users")
    .then(function (r) { return r.json(); })
    .then(function (users) {
      lastOidcUsers = users || [];
      renderOidcUsersTable(lastOidcUsers);
    })
    .catch(function () {});
}

function renderOidcUsersTable(users) {
  document.getElementById("oidc-users-body").innerHTML = users.map(function (u) {
    const hasView = u.permissions.indexOf("view") !== -1;
    const hasDownload = u.permissions.indexOf("download") !== -1;
    const hasLoginLog = u.permissions.indexOf("login-log") !== -1;
    const hasDownloadLog = u.permissions.indexOf("download-log") !== -1;
    const hasAdmin = u.permissions.indexOf("admin") !== -1;
    const identity = escapeHtml(u.identity);

    return '<tr>' +
      '<td class="username">' + identity + '</td>' +
      '<td><input type="checkbox" class="oidc-user-perm" data-identity="' + identity + '" data-perm="view"' + (hasView ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="oidc-user-perm" data-identity="' + identity + '" data-perm="download"' + (hasDownload ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="oidc-user-perm" data-identity="' + identity + '" data-perm="login-log"' + (hasLoginLog ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="oidc-user-perm" data-identity="' + identity + '" data-perm="download-log"' + (hasDownloadLog ? ' checked' : '') + '></td>' +
      '<td><input type="checkbox" class="oidc-user-perm" data-identity="' + identity + '" data-perm="admin"' + (hasAdmin ? ' checked' : '') + '></td>' +
      '<td>' + fmtTime(u.updated_at) + '</td>' +
      '<td><a href="#" class="remove-oidc-user-link" data-identity="' + identity + '">remove</a></td>' +
      '</tr>';
  }).join("");
}

// A permission checkbox's change re-submits that row's whole permission
// set as a PUT, same as the "Users" table's own handler above.
document.getElementById("oidc-users-body").addEventListener("change", function (e) {
  const box = e.target.closest(".oidc-user-perm");
  if (!box) return;

  const row = box.closest("tr");
  const perms = [];
  row.querySelectorAll(".oidc-user-perm").forEach(function (input) {
    if (input.checked) perms.push(input.dataset.perm);
  });

  apiFetch("/api/oidc-users/" + encodeURIComponent(box.dataset.identity), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ permissions: perms })
  }).catch(function () {});
});

const deleteOidcUserDialog = document.getElementById("delete-oidc-user-confirm-dialog");
const deleteOidcUserDialogIdentity = document.getElementById("delete-oidc-user-confirm-identity");
let pendingOidcUserDelete = null;

deleteOidcUserDialog.addEventListener("close", function () {
  if (deleteOidcUserDialog.returnValue === "confirm" && pendingOidcUserDelete) {
    apiFetch("/api/oidc-users/" + encodeURIComponent(pendingOidcUserDelete), { method: "DELETE" })
      .then(function () { loadOidcUsers(); })
      .catch(function () {});
  }
  pendingOidcUserDelete = null;
});

document.getElementById("oidc-users-body").addEventListener("click", function (e) {
  const link = e.target.closest(".remove-oidc-user-link");
  if (!link) return;

  e.preventDefault();
  pendingOidcUserDelete = link.dataset.identity;
  deleteOidcUserDialogIdentity.textContent = link.dataset.identity;
  deleteOidcUserDialog.showModal();
});

// Setting an override posts the form as JSON, same as "Add user" above —
// PUT /api/oidc-users/{identity} is an upsert, so this both creates a new
// override and edits an existing one.
document.getElementById("add-oidc-user-form").addEventListener("submit", function (e) {
  e.preventDefault();

  const identityInput = document.getElementById("add-oidc-user-identity");
  const viewInput = document.getElementById("add-oidc-user-view");
  const downloadInput = document.getElementById("add-oidc-user-download");
  const loginLogInput = document.getElementById("add-oidc-user-login-log");
  const downloadLogInput = document.getElementById("add-oidc-user-download-log");
  const adminInput = document.getElementById("add-oidc-user-admin");
  const errEl = document.getElementById("add-oidc-user-error");

  const perms = [];
  if (viewInput.checked) perms.push("view");
  if (downloadInput.checked) perms.push("download");
  if (loginLogInput.checked) perms.push("login-log");
  if (downloadLogInput.checked) perms.push("download-log");
  if (adminInput.checked) perms.push("admin");

  errEl.hidden = true;

  const identity = identityInput.value.trim();

  apiFetch("/api/oidc-users/" + encodeURIComponent(identity), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ permissions: perms })
  }).then(function (r) {
    if (!r.ok) {
      return r.text().then(function (msg) {
        throw new Error(msg || "setting permissions failed");
      });
    }

    identityInput.value = "";
    viewInput.checked = true;
    downloadInput.checked = false;
    loginLogInput.checked = false;
    downloadLogInput.checked = false;
    adminInput.checked = false;
    loadOidcUsers();
  }).catch(function (err) {
    errEl.textContent = err.message || "setting permissions failed";
    errEl.hidden = false;
  });
});

function refresh() {
  if (!identityLoaded) loadIdentity();
  if (!sessionInfoLoaded) loadSessionInfo();

  loadLoginEvents();
  loadDownloadEvents();

  Promise.all([
    apiFetch("/api/status").then(function (r) { return r.json(); }),
    apiFetch("/api/receivers").then(function (r) { return r.json(); }),
    apiFetch("/api/logs").then(function (r) { return r.json(); })
  ]).then(function (results) {
    render(results[0]);
    lastReceivers = results[1] || [];
    renderReceivers(lastReceivers);
    renderLogs(results[2]);
    document.getElementById("updated").textContent = "updated " + new Date().toLocaleTimeString();
  }).catch(function (err) {
    document.getElementById("updated").textContent = "error fetching status: " + err;
  });
}

refresh();
setInterval(refresh, 2000);
