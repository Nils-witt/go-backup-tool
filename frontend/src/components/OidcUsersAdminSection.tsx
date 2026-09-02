import { useEffect, useState, type FormEvent } from "react";
import { apiFetch, apiFetchJSON, apiFetchOK } from "../api/client";
import type { OidcUserPermissionJSON } from "../api/types";
import { ConfirmDialog } from "./Dialog";
import { fmtTime } from "../lib/format";

const PERM_COLUMNS = [
  { key: "view", label: "View" },
  { key: "download", label: "Download" },
  { key: "login-log", label: "Login log" },
  { key: "download-log", label: "Download log" },
  { key: "admin", label: "Admin" },
];

// OidcUsersAdminSection ports the "OIDC users" admin panel (dashboard.js:
// 910-1050): per-identity permission overrides for SSO logins. Gated on
// sessionInfo.admin && sessionInfo.oidc_enabled by the caller via `visible`.
export function OidcUsersAdminSection({ visible }: { visible: boolean }) {
  const [users, setUsers] = useState<OidcUserPermissionJSON[]>([]);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  function loadUsers() {
    apiFetchJSON<OidcUserPermissionJSON[]>("/api/oidc-users")
      .then((u) => setUsers(u || []))
      .catch(() => {});
  }

  useEffect(() => {
    if (visible) loadUsers();
  }, [visible]);

  function setPermission(identity: string, perm: string, checked: boolean, current: string[]) {
    const perms = checked ? [...current, perm] : current.filter((p) => p !== perm);
    apiFetch("/api/oidc-users/" + encodeURIComponent(identity), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ permissions: perms }),
    }).catch(() => {});
  }

  if (!visible) return null;

  return (
    <div id="oidc-users-wrap">
      <h2 className="section-title">OIDC users</h2>
      <div className="card users-card">
        <p className="section-hint">
          Per-identity permission overrides for SSO logins. An identity with no override here gets the configured
          default permissions.
        </p>
        <table className="users-table">
          <thead>
            <tr>
              <th>Identity</th>
              <th>View</th>
              <th>Download</th>
              <th>Login log</th>
              <th>Download log</th>
              <th>Admin</th>
              <th>Updated</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => (
              <tr key={u.identity}>
                <td className="username">{u.identity}</td>
                {PERM_COLUMNS.map((col) => (
                  <td key={col.key}>
                    <input
                      type="checkbox"
                      checked={u.permissions.includes(col.key)}
                      onChange={(e) => setPermission(u.identity, col.key, e.target.checked, u.permissions)}
                    />
                  </td>
                ))}
                <td>{fmtTime(u.updated_at)}</td>
                <td>
                  <a
                    href="#"
                    className="remove-oidc-user-link"
                    onClick={(e) => {
                      e.preventDefault();
                      setPendingDelete(u.identity);
                    }}
                  >
                    remove
                  </a>
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        <AddOidcUserForm onAdded={loadUsers} />
      </div>

      <ConfirmDialog
        open={pendingDelete !== null}
        message={
          <>
            Remove permission override for <strong>{pendingDelete}</strong>?
          </>
        }
        confirmLabel="Remove"
        onConfirm={() => {
          if (pendingDelete) {
            apiFetch("/api/oidc-users/" + encodeURIComponent(pendingDelete), { method: "DELETE" })
              .then(() => loadUsers())
              .catch(() => {});
          }
          setPendingDelete(null);
        }}
        onCancel={() => setPendingDelete(null)}
      />
    </div>
  );
}

function AddOidcUserForm({ onAdded }: { onAdded: () => void }) {
  const [identity, setIdentity] = useState("");
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

    const id = identity.trim();

    apiFetchOK(
      "/api/oidc-users/" + encodeURIComponent(id),
      { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ permissions: perms }) },
      "setting permissions failed",
    )
      .then(() => {
        setIdentity("");
        setView(true);
        setDownload(false);
        setLoginLog(false);
        setDownloadLog(false);
        setAdmin(false);
        onAdded();
      })
      .catch((err: Error) => setError(err.message || "setting permissions failed"));
  }

  return (
    <>
      <form className="add-user-form" onSubmit={submit}>
        <input
          type="text"
          placeholder="Email or subject"
          autoComplete="off"
          required
          value={identity}
          onChange={(e) => setIdentity(e.target.value)}
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
          Set permissions
        </button>
      </form>
      {error ? <p className="err">{error}</p> : null}
    </>
  );
}
