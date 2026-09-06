import { useEffect, useState, type FormEvent } from "react";
import { apiFetch, apiFetchJSON, apiFetchOK } from "../api/client";
import type { WebUIGroupJSON } from "../api/types";
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
  { key: "job-run-log", label: "Job run log" },
  { key: "target-run-log", label: "Target run log" },
  { key: "receiver-log", label: "Receiver log" },
  { key: "admin", label: "Admin" },
];

// GroupsAdminSection is the "Users" admin section's group management panel:
// a table of groups with permission checkboxes (each change re-submits that
// group's whole permission set, matching UsersAdminSection's own checkbox
// behavior), an editable OIDC group mapping per row (see oidcGroupName on
// the Go side), and an add-group form. Assigning users to a group happens on
// the Users table itself (see UsersAdminSection), not here.
export function GroupsAdminSection() {
  const [groups, setGroups] = useState<WebUIGroupJSON[]>([]);
  const [oidcGroupNameDrafts, setOidcGroupNameDrafts] = useState<Record<string, string>>({});
  const [oidcGroupNameErrors, setOidcGroupNameErrors] = useState<Record<string, string>>({});
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  function loadGroups() {
    apiFetchJSON<WebUIGroupJSON[]>("/api/groups")
      .then((g) => {
        setGroups(g || []);
        setOidcGroupNameDrafts(Object.fromEntries((g || []).map((x) => [x.name, x.oidc_group_name])));
      })
      .catch(() => {});
  }

  useEffect(() => {
    loadGroups();
  }, []);

  function setPermission(g: WebUIGroupJSON, perm: string, checked: boolean) {
    const perms = checked ? [...g.permissions, perm] : g.permissions.filter((p) => p !== perm);
    apiFetch("/api/groups/" + encodeURIComponent(g.name), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ permissions: perms, oidc_group_name: g.oidc_group_name }),
    })
      .then(() => loadGroups())
      .catch(() => {});
  }

  function submitOidcGroupName(g: WebUIGroupJSON) {
    const oidcGroupName = (oidcGroupNameDrafts[g.name] ?? "").trim();
    setOidcGroupNameErrors((prev) => ({ ...prev, [g.name]: "" }));

    if (oidcGroupName === g.oidc_group_name) {
      return;
    }

    apiFetchOK(
      "/api/groups/" + encodeURIComponent(g.name),
      {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ permissions: g.permissions, oidc_group_name: oidcGroupName }),
      },
      "mapping oidc group failed",
    )
      .then(() => loadGroups())
      .catch((err: Error) => {
        setOidcGroupNameErrors((prev) => ({
          ...prev,
          [g.name]: err.message || "mapping oidc group failed",
        }));
        setOidcGroupNameDrafts((prev) => ({ ...prev, [g.name]: g.oidc_group_name }));
      });
  }

  return (
    <Stack spacing={2}>
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Name</TableCell>
              <TableCell>OIDC group</TableCell>
              {PERM_COLUMNS.map((col) => (
                <TableCell key={col.key}>{col.label}</TableCell>
              ))}
              <TableCell>Created</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {groups.map((g) => (
              <TableRow key={g.name}>
                <TableCell sx={{ overflowWrap: "anywhere" }}>{g.name}</TableCell>
                <TableCell sx={{ minWidth: 160 }}>
                  <TextField
                    size="small"
                    variant="standard"
                    placeholder="unmapped"
                    fullWidth
                    error={!!oidcGroupNameErrors[g.name]}
                    helperText={oidcGroupNameErrors[g.name] || undefined}
                    value={oidcGroupNameDrafts[g.name] ?? ""}
                    onChange={(e) =>
                      setOidcGroupNameDrafts((prev) => ({ ...prev, [g.name]: e.target.value }))
                    }
                    onBlur={() => submitOidcGroupName(g)}
                  />
                </TableCell>
                {PERM_COLUMNS.map((col) => (
                  <TableCell key={col.key}>
                    <Checkbox
                      size="small"
                      checked={g.permissions.includes(col.key)}
                      onChange={(e) => setPermission(g, col.key, e.target.checked)}
                    />
                  </TableCell>
                ))}
                <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(g.created_at)}</TableCell>
                <TableCell sx={{ whiteSpace: "nowrap" }}>
                  <Link
                    component="button"
                    variant="body2"
                    color="error"
                    underline="hover"
                    onClick={() => setPendingDelete(g.name)}
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
        <AddGroupForm onAdded={loadGroups} />
      </Paper>

      <ConfirmDialog
        open={pendingDelete !== null}
        message={
          <>
            Remove group <strong>{pendingDelete}</strong>? Every member loses whatever permissions
            it granted.
          </>
        }
        confirmLabel="Remove"
        onConfirm={() => {
          if (pendingDelete) {
            apiFetch("/api/groups/" + encodeURIComponent(pendingDelete), { method: "DELETE" })
              .then(() => loadGroups())
              .catch(() => {});
          }
          setPendingDelete(null);
        }}
        onCancel={() => setPendingDelete(null)}
      />
    </Stack>
  );
}

function AddGroupForm({ onAdded }: { onAdded: () => void }) {
  const [name, setName] = useState("");
  const [oidcGroupName, setOidcGroupName] = useState("");
  const [view, setView] = useState(true);
  const [download, setDownload] = useState(false);
  const [loginLog, setLoginLog] = useState(false);
  const [downloadLog, setDownloadLog] = useState(false);
  const [jobRunLog, setJobRunLog] = useState(false);
  const [targetRunLog, setTargetRunLog] = useState(false);
  const [receiverLog, setReceiverLog] = useState(false);
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
    if (jobRunLog) perms.push("job-run-log");
    if (targetRunLog) perms.push("target-run-log");
    if (receiverLog) perms.push("receiver-log");
    if (admin) perms.push("admin");

    apiFetchOK(
      "/api/groups",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          name: name.trim(),
          permissions: perms,
          oidc_group_name: oidcGroupName.trim(),
        }),
      },
      "adding group failed",
    )
      .then(() => {
        setName("");
        setOidcGroupName("");
        setView(true);
        setDownload(false);
        setLoginLog(false);
        setDownloadLog(false);
        setJobRunLog(false);
        setTargetRunLog(false);
        setReceiverLog(false);
        setAdmin(false);
        onAdded();
      })
      .catch((err: Error) => setError(err.message || "adding group failed"));
  }

  return (
    <Box component="form" onSubmit={submit}>
      <Typography sx={{ fontWeight: 600, mb: 1.5 }}>Add group</Typography>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap", alignItems: "center" }}>
        <TextField
          size="small"
          placeholder="Group name"
          autoComplete="off"
          required
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <TextField
          size="small"
          placeholder="OIDC group (optional)"
          autoComplete="off"
          value={oidcGroupName}
          onChange={(e) => setOidcGroupName(e.target.value)}
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
            <Checkbox
              size="small"
              checked={jobRunLog}
              onChange={(e) => setJobRunLog(e.target.checked)}
            />
          }
          label="Job run log"
        />
        <FormControlLabel
          control={
            <Checkbox
              size="small"
              checked={targetRunLog}
              onChange={(e) => setTargetRunLog(e.target.checked)}
            />
          }
          label="Target run log"
        />
        <FormControlLabel
          control={
            <Checkbox
              size="small"
              checked={receiverLog}
              onChange={(e) => setReceiverLog(e.target.checked)}
            />
          }
          label="Receiver log"
        />
        <FormControlLabel
          control={
            <Checkbox size="small" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
          }
          label="Admin"
        />
        <Button type="submit" variant="contained">
          Add group
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
