import { useEffect, useState, type FormEvent } from "react";
import { apiFetch, apiFetchJSON, apiFetchOK } from "../api/client";
import type { OidcUserPermissionJSON } from "../api/types";
import { ConfirmDialog } from "./ConfirmDialog";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Checkbox from "@mui/material/Checkbox";
import FormControlLabel from "@mui/material/FormControlLabel";
import Link from "@mui/material/Link";
import Paper from "@mui/material/Paper";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

const PERM_COLUMNS = [
  { key: "view", label: "View" },
  { key: "download", label: "Download" },
  { key: "login-log", label: "Login log" },
  { key: "download-log", label: "Download log" },
  { key: "admin", label: "Admin" },
];

// OidcUsersAdminSection is the "OIDC users" admin panel: per-identity
// permission overrides for SSO logins.
export function OidcUsersAdminSection() {
  const [users, setUsers] = useState<OidcUserPermissionJSON[]>([]);
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  function loadUsers() {
    apiFetchJSON<OidcUserPermissionJSON[]>("/api/oidc-users")
      .then((u) => setUsers(u || []))
      .catch(() => {});
  }

  useEffect(() => {
    loadUsers();
  }, []);

  function setPermission(identity: string, perm: string, checked: boolean, current: string[]) {
    const perms = checked ? [...current, perm] : current.filter((p) => p !== perm);
    apiFetch("/api/oidc-users/" + encodeURIComponent(identity), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ permissions: perms }),
    }).catch(() => {});
  }

  return (
    <Stack spacing={2}>
      <Typography variant="body2" color="text.secondary">
        Per-identity permission overrides for SSO logins. An identity with no override here gets the
        configured default permissions.
      </Typography>
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Identity</TableCell>
              {PERM_COLUMNS.map((col) => (
                <TableCell key={col.key}>{col.label}</TableCell>
              ))}
              <TableCell>Updated</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {users.map((u) => (
              <TableRow key={u.identity}>
                <TableCell sx={{ overflowWrap: "anywhere" }}>{u.identity}</TableCell>
                {PERM_COLUMNS.map((col) => (
                  <TableCell key={col.key}>
                    <Checkbox
                      size="small"
                      checked={u.permissions.includes(col.key)}
                      onChange={(e) =>
                        setPermission(u.identity, col.key, e.target.checked, u.permissions)
                      }
                    />
                  </TableCell>
                ))}
                <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(u.updated_at)}</TableCell>
                <TableCell sx={{ whiteSpace: "nowrap" }}>
                  <Link
                    component="button"
                    variant="body2"
                    color="error"
                    underline="hover"
                    onClick={() => setPendingDelete(u.identity)}
                  >
                    remove
                  </Link>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>

      <Paper variant="outlined" sx={{ p: 2 }}>
        <AddOidcUserForm onAdded={loadUsers} />
      </Paper>

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
    </Stack>
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
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ permissions: perms }),
      },
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
    <Box component="form" onSubmit={submit}>
      <Typography sx={{ fontWeight: 600, mb: 1.5 }}>Add override</Typography>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap", alignItems: "center" }}>
        <TextField
          size="small"
          placeholder="Email or subject"
          autoComplete="off"
          required
          value={identity}
          onChange={(e) => setIdentity(e.target.value)}
        />
        <FormControlLabel
          control={
            <Checkbox size="small" checked={view} onChange={(e) => setView(e.target.checked)} />
          }
          label="View"
        />
        <FormControlLabel
          control={
            <Checkbox
              size="small"
              checked={download}
              onChange={(e) => setDownload(e.target.checked)}
            />
          }
          label="Download"
        />
        <FormControlLabel
          control={
            <Checkbox
              size="small"
              checked={loginLog}
              onChange={(e) => setLoginLog(e.target.checked)}
            />
          }
          label="Login log"
        />
        <FormControlLabel
          control={
            <Checkbox
              size="small"
              checked={downloadLog}
              onChange={(e) => setDownloadLog(e.target.checked)}
            />
          }
          label="Download log"
        />
        <FormControlLabel
          control={
            <Checkbox size="small" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
          }
          label="Admin"
        />
        <Button type="submit" variant="contained">
          Set permissions
        </Button>
      </Stack>
      {error ? (
        <Alert severity="error" sx={{ mt: 1.5 }}>
          {error}
        </Alert>
      ) : null}
    </Box>
  );
}
