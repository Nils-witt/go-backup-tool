import { useEffect, useState, type FormEvent } from "react";
import { apiFetch, apiFetchJSON, apiFetchOK } from "../api/client";
import type { WebUIUserJSON } from "../api/types";
import { ConfirmDialog } from "./ConfirmDialog";
import { fmtTime } from "../lib/format";
import { UserTokensDialog } from "./UserTokensDialog";
import { TokenResultDialog, type TokenResult } from "./TokenResultDialog";
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

// UsersAdminSection is the "Users" admin CRUD panel: the table of
// web-UI-managed accounts, permission checkboxes (each change re-submits
// that row's whole permission set, since the API takes the full set rather
// than a single add/remove), add-user form, and per-user API token
// issuance/management.
export function UsersAdminSection() {
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
    loadUsers();
  }, []);

  function setPermission(username: string, perm: string, checked: boolean, current: string[]) {
    const perms = checked ? [...current, perm] : current.filter((p) => p !== perm);
    apiFetch("/api/users/" + encodeURIComponent(username), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ permissions: perms }),
    }).catch(() => {});
  }

  return (
    <Stack spacing={2}>
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Username</TableCell>
              {PERM_COLUMNS.map((col) => (
                <TableCell key={col.key}>{col.label}</TableCell>
              ))}
              <TableCell>Created</TableCell>
              <TableCell />
              <TableCell />
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {users.map((u) => (
              <TableRow key={u.username}>
                <TableCell sx={{ overflowWrap: "anywhere" }}>{u.username}</TableCell>
                {PERM_COLUMNS.map((col) => (
                  <TableCell key={col.key}>
                    <Checkbox
                      size="small"
                      checked={u.permissions.includes(col.key)}
                      onChange={(e) =>
                        setPermission(u.username, col.key, e.target.checked, u.permissions)
                      }
                    />
                  </TableCell>
                ))}
                <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(u.created_at)}</TableCell>
                <TableCell sx={{ whiteSpace: "nowrap" }}>
                  <Link
                    component="button"
                    variant="body2"
                    underline="hover"
                    onClick={() => {
                      setIssueTokenDays("365");
                      setIssueTokenUser(u.username);
                    }}
                  >
                    issue token
                  </Link>
                </TableCell>
                <TableCell sx={{ whiteSpace: "nowrap" }}>
                  <Link
                    component="button"
                    variant="body2"
                    underline="hover"
                    onClick={() => setTokensUser(u.username)}
                  >
                    tokens
                  </Link>
                </TableCell>
                <TableCell sx={{ whiteSpace: "nowrap" }}>
                  <Link
                    component="button"
                    variant="body2"
                    color="error"
                    underline="hover"
                    onClick={() => setPendingDelete(u.username)}
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
        <AddUserForm onAdded={loadUsers} />
      </Paper>

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
            Issue a long-lived API token for <strong>{issueTokenUser}</strong>, carrying that user's
            current permissions.
          </>
        }
        confirmLabel="Issue"
        onConfirm={() => {
          const username = issueTokenUser as string;
          const days = parseInt(issueTokenDays, 10) || 365;
          setIssueTokenUser(null);

          apiFetchOK(
            "/api/users/" + encodeURIComponent(username) + "/tokens",
            {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ days }),
            },
            "issuing token failed",
          )
            .then((r) => r.json())
            .then((resp: { token: string; expires_at: string }) => {
              setTokenResult({
                username,
                expiresAt: resp.expires_at,
                token: resp.token,
                error: "",
              });
            })
            .catch((err: Error) => {
              setTokenResult({
                username,
                expiresAt: "",
                token: "",
                error: err.message || "issuing token failed",
              });
            });
        }}
        onCancel={() => setIssueTokenUser(null)}
      >
        <TextField
          label="Valid for (days)"
          type="number"
          size="small"
          slotProps={{ htmlInput: { min: 1, max: 3650 } }}
          value={issueTokenDays}
          onChange={(e) => setIssueTokenDays(e.target.value)}
          required
        />
      </ConfirmDialog>

      <UserTokensDialog username={tokensUser} onClose={() => setTokensUser(null)} />
      <TokenResultDialog result={tokenResult} onClose={() => setTokenResult(null)} />
    </Stack>
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
    <Box component="form" onSubmit={submit}>
      <Typography sx={{ fontWeight: 600, mb: 1.5 }}>Add user</Typography>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap", alignItems: "center" }}>
        <TextField
          size="small"
          placeholder="Username"
          autoComplete="off"
          required
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <TextField
          size="small"
          type="password"
          placeholder="Password"
          autoComplete="new-password"
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
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
          Add user
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
