import { useEffect, useState, type FormEvent } from "react";
import { apiFetch, apiFetchJSON, apiFetchOK } from "../api/client";
import type { WebUIUserJSON } from "../api/types";
import { ConfirmDialog } from "./Dialog";
import { fmtTime } from "../lib/format";
import { UserTokensDialog } from "./UserTokensDialog";
import { TokenResultDialog, type TokenResult } from "./TokenResultDialog";

const PERM_COLUMNS = [
  { key: "view", label: "View" },
  { key: "download", label: "Download" },
  { key: "login-log", label: "Login log" },
  { key: "download-log", label: "Download log" },
  { key: "admin", label: "Admin" },
];

// UsersAdminSection ports the "Users" admin CRUD panel (dashboard.js:637-908):
// the table of web-UI-managed accounts, permission checkboxes (each change
// re-submits that row's whole permission set, since the API takes the full
// set rather than a single add/remove), add-user form, and per-user API
// token issuance/management. Gated on sessionInfo.admin by the caller via
// `visible` — mirroring renderUsersSection's client-side gate of the same
// server-enforced rule (requireAdmin in webui.go).
export function UsersAdminSection({ visible }: { visible: boolean }) {
  const [users, setUsers] = useState<WebUIUserJSON[]>([]);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);
  const [tokensUser, setTokensUser] = useState<string | null>(null);
  const [issueTokenUser, setIssueTokenUser] = useState<string | null>(null);
  const [issueTokenDays, setIssueTokenDays] = useState("365");
  const [tokenResult, setTokenResult] = useState<TokenResult | null>(null);

  function loadUsers() {
    apiFetchJSON<WebUIUserJSON[]>("/api/users")
      .then((u) => setUsers(u || []))
      .catch(() => {});
  }

  useEffect(() => {
    if (visible) loadUsers();
  }, [visible]);

  function setPermission(username: string, perm: string, checked: boolean, current: string[]) {
    const perms = checked ? [...current, perm] : current.filter((p) => p !== perm);
    apiFetch("/api/users/" + encodeURIComponent(username), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ permissions: perms }),
    }).catch(() => {});
  }

  if (!visible) return null;

  return (
    <div id="users-wrap">
      <h2 className="section-title">Users</h2>
      <div className="card users-card">
        <table className="users-table">
          <thead>
            <tr>
              <th>Username</th>
              <th>View</th>
              <th>Download</th>
              <th>Login log</th>
              <th>Download log</th>
              <th>Admin</th>
              <th>Created</th>
              <th></th>
              <th></th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.username}>
                <td className="username">{u.username}</td>
                {PERM_COLUMNS.map((col) => (
                  <td key={col.key}>
                    <input
                      type="checkbox"
                      checked={u.permissions.includes(col.key)}
                      onChange={(e) => setPermission(u.username, col.key, e.target.checked, u.permissions)}
                    />
                  </td>
                ))}
                <td>{fmtTime(u.created_at)}</td>
                <td>
                  <a
                    href="#"
                    className="issue-token-link"
                    onClick={(e) => {
                      e.preventDefault();
                      setIssueTokenDays("365");
                      setIssueTokenUser(u.username);
                    }}
                  >
                    issue token
                  </a>
                </td>
                <td>
                  <a
                    href="#"
                    className="manage-tokens-link"
                    onClick={(e) => {
                      e.preventDefault();
                      setTokensUser(u.username);
                    }}
                  >
                    tokens
                  </a>
                </td>
                <td>
                  <a
                    href="#"
                    className="remove-user-link"
                    onClick={(e) => {
                      e.preventDefault();
                      setPendingDelete(u.username);
                    }}
                  >
                    remove
                  </a>
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        <AddUserForm onAdded={loadUsers} />
      </div>

      <ConfirmDialog
        open={pendingDelete !== null}
        message={
          <>
            Remove user <strong>{pendingDelete}</strong>?
          </>
        }
        confirmLabel="Remove"
        onConfirm={() => {
          if (pendingDelete) {
            apiFetch("/api/users/" + encodeURIComponent(pendingDelete), { method: "DELETE" })
              .then(() => loadUsers())
              .catch(() => {});
          }
          setPendingDelete(null);
        }}
        onCancel={() => setPendingDelete(null)}
      />

      <ConfirmDialog
        open={issueTokenUser !== null}
        message={
          <>
            Issue a long-lived API token for <strong>{issueTokenUser}</strong>, carrying that user's current
            permissions.
          </>
        }
        confirmLabel="Issue"
        onConfirm={() => {
          const username = issueTokenUser as string;
          const days = parseInt(issueTokenDays, 10) || 365;
          setIssueTokenUser(null);

          apiFetchOK(
            "/api/users/" + encodeURIComponent(username) + "/tokens",
            { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ days }) },
            "issuing token failed",
          )
            .then((r) => r.json())
            .then((resp: { token: string; expires_at: string }) => {
              setTokenResult({ username, expiresAt: resp.expires_at, token: resp.token, error: "" });
            })
            .catch((err: Error) => {
              setTokenResult({ username, expiresAt: "", token: "", error: err.message || "issuing token failed" });
            });
        }}
        onCancel={() => setIssueTokenUser(null)}
      >
        <label className="token-days-label">
          Valid for
          <input
            type="number"
            min={1}
            max={3650}
            value={issueTokenDays}
            onChange={(e) => setIssueTokenDays(e.target.value)}
            required
          />
          days
        </label>
      </ConfirmDialog>

      <UserTokensDialog username={tokensUser} onClose={() => setTokensUser(null)} />
      <TokenResultDialog result={tokenResult} onClose={() => setTokenResult(null)} />
    </div>
  );
}

function AddUserForm({ onAdded }: { onAdded: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [view, setView] = useState(true);
  const [download, setDownload] = useState(false);
  const [loginLog, setLoginLog] = useState(false);
  const [downloadLog, setDownloadLog] = useState(false);
  const [admin, setAdmin] = useState(false);
  const [error, setError] = useState("");

  function submit(e: FormEvent) {
    e.preventDefault();
    setError("");

    const perms: string[] = [];
    if (view) perms.push("view");
    if (download) perms.push("download");
    if (loginLog) perms.push("login-log");
    if (downloadLog) perms.push("download-log");
    if (admin) perms.push("admin");

    apiFetchOK(
      "/api/users",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: username.trim(), password, permissions: perms }),
      },
      "adding user failed",
    )
      .then(() => {
        setUsername("");
        setPassword("");
        setView(true);
        setDownload(false);
        setLoginLog(false);
        setDownloadLog(false);
        setAdmin(false);
        onAdded();
      })
      .catch((err: Error) => setError(err.message || "adding user failed"));
  }

  return (
    <>
      <form className="add-user-form" onSubmit={submit}>
        <input
          type="text"
          placeholder="Username"
          autoComplete="off"
          required
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <input
          type="password"
          placeholder="Password"
          autoComplete="new-password"
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <label>
          <input type="checkbox" checked={view} onChange={(e) => setView(e.target.checked)} /> View
        </label>
        <label>
          <input type="checkbox" checked={download} onChange={(e) => setDownload(e.target.checked)} /> Download
        </label>
        <label>
          <input type="checkbox" checked={loginLog} onChange={(e) => setLoginLog(e.target.checked)} /> Login log
        </label>
        <label>
          <input type="checkbox" checked={downloadLog} onChange={(e) => setDownloadLog(e.target.checked)} /> Download
          log
        </label>
        <label>
          <input type="checkbox" checked={admin} onChange={(e) => setAdmin(e.target.checked)} /> Admin
        </label>
        <button type="submit" className="btn btn-primary">
          Add user
        </button>
      </form>
      {error ? <p className="err">{error}</p> : null}
    </>
  );
}
