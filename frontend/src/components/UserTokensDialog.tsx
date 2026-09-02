import { useEffect, useState } from "react";
import { apiFetch, apiFetchJSON } from "../api/client";
import type { ApiTokenJSON } from "../api/types";
import { Badge } from "./Badge";
import { Dialog } from "./Dialog";
import { fmtTime } from "../lib/format";

// UserTokensDialog lists every long-lived API token recorded for username,
// with a "revoke" link per still-active one — mirrors tokens-dialog in
// dashboard.html.
export function UserTokensDialog({ username, onClose }: { username: string | null; onClose: () => void }) {
  const [tokens, setTokens] = useState<ApiTokenJSON[]>([]);

  function load(name: string) {
    apiFetchJSON<ApiTokenJSON[]>("/api/users/" + encodeURIComponent(name) + "/tokens")
      .then((t) => setTokens(t || []))
      .catch(() => setTokens([]));
  }

  useEffect(() => {
    if (username) {
      setTokens([]);
      load(username);
    }
  }, [username]);

  function revoke(jti: string) {
    if (!username) return;
    apiFetch("/api/users/" + encodeURIComponent(username) + "/tokens/" + encodeURIComponent(jti), { method: "DELETE" })
      .then(() => load(username))
      .catch(() => {});
  }

  return (
    <Dialog open={username !== null} onClose={() => onClose()}>
      <form method="dialog">
        <p className="confirm-message">
          API tokens issued for <strong>{username}</strong>.
        </p>
        <table className="users-table">
          <thead>
            <tr>
              <th>Issued</th>
              <th>Expires</th>
              <th>Status</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {tokens.map((tok) => (
              <tr key={tok.jti}>
                <td>{fmtTime(tok.created_at)}</td>
                <td>{fmtTime(tok.expires_at)}</td>
                <td>{tok.revoked ? <Badge state="failed" label="revoked" /> : <Badge state="ok" label="active" />}</td>
                <td>
                  {tok.revoked ? null : (
                    <a
                      href="#"
                      className="revoke-token-link"
                      onClick={(e) => {
                        e.preventDefault();
                        revoke(tok.jti);
                      }}
                    >
                      revoke
                    </a>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className="confirm-actions">
          <button type="submit" value="ok" className="btn btn-primary" autoFocus>
            Close
          </button>
        </div>
      </form>
    </Dialog>
  );
}
