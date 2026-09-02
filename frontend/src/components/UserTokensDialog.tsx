import { useEffect, useState } from "react";
import { apiFetch, apiFetchJSON } from "../api/client";
import type { ApiTokenJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { fmtTime } from "../lib/format";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogContentText from "@mui/material/DialogContentText";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Link from "@mui/material/Link";

// UserTokensDialog lists every long-lived API token recorded for username,
// with a "revoke" link per still-active one.
export function UserTokensDialog({
  username,
  onClose,
}: {
  username: string | null;
  onClose: () => void;
}) {
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
    apiFetch("/api/users/" + encodeURIComponent(username) + "/tokens/" + encodeURIComponent(jti), {
      method: "DELETE",
    })
      .then(() => load(username))
      .catch(() => {});
  }

  return (
    <Dialog open={username !== null} onClose={onClose} fullWidth maxWidth="sm">
      <DialogContent>
        <DialogContentText sx={{ mb: 2 }}>
          API tokens issued for <strong>{username}</strong>.
        </DialogContentText>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Issued</TableCell>
              <TableCell>Expires</TableCell>
              <TableCell>Status</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {tokens.map((tok) => (
              <TableRow key={tok.jti}>
                <TableCell>{fmtTime(tok.created_at)}</TableCell>
                <TableCell>{fmtTime(tok.expires_at)}</TableCell>
                <TableCell>
                  {tok.revoked ? (
                    <StatusChip state="failed" label="revoked" />
                  ) : (
                    <StatusChip state="ok" label="active" />
                  )}
                </TableCell>
                <TableCell>
                  {tok.revoked ? null : (
                    <Link
                      component="button"
                      color="error"
                      underline="hover"
                      variant="body2"
                      onClick={() => revoke(tok.jti)}
                    >
                      revoke
                    </Link>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} variant="contained" autoFocus>
          Close
        </Button>
      </DialogActions>
    </Dialog>
  );
}
