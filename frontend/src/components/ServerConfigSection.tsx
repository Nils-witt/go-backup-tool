import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { ServerConfigJSON, ServerConfigListJSON, ServerDefinitionJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { EditingDisabledNotice } from "./EditingDisabledNotice";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import IconButton from "@mui/material/IconButton";
import MenuItem from "@mui/material/MenuItem";
import Paper from "@mui/material/Paper";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TextField from "@mui/material/TextField";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import AddIcon from "@mui/icons-material/Add";
import DeleteIcon from "@mui/icons-material/Delete";
import EditIcon from "@mui/icons-material/Edit";

const EMPTY_FORM: ServerDefinitionJSON = {
  name: "",
  type: "local",
  endpoint: "",
  path: "",
  retention: "",
};

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// definitionOf keeps only the fields the server's type uses, so switching
// type in the dialog doesn't send the other type's leftovers.
function definitionOf(form: ServerDefinitionJSON): ServerDefinitionJSON {
  const name = form.name.trim();
  return form.type === "remote"
    ? { name, type: "remote", endpoint: form.endpoint.trim(), path: "", retention: "" }
    : {
        name,
        type: "local",
        endpoint: "",
        path: form.path.trim(),
        retention: form.retention.trim(),
      };
}

function ServerDialog({
  editing,
  form,
  saving,
  error,
  onChange,
  onCancel,
  onSave,
}: {
  editing: ServerConfigJSON | null;
  form: ServerDefinitionJSON;
  saving: boolean;
  error: string | null;
  onChange: (f: ServerDefinitionJSON) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<ServerDefinitionJSON>) => onChange({ ...form, ...patch });
  const local = form.type !== "remote";
  const canSave = !!form.name.trim() && (local ? !!form.path.trim() : !!form.endpoint.trim());

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="sm">
      <DialogTitle>{editing ? `Edit server ${editing.name}` : "New server"}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <TextField
            label="Name"
            value={form.name}
            onChange={(e) => set({ name: e.target.value })}
            helperText="What a job's targets name. Can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
          />
          <TextField
            select
            label="Type"
            value={form.type}
            onChange={(e) => set({ type: e.target.value })}
            helperText={
              local
                ? "A directory on this machine. Objects go to path/bucket/key."
                : "Another go-backup-tool instance's receiver API. The bucket names its receiver."
            }
          >
            <MenuItem value="local">Local directory</MenuItem>
            <MenuItem value="remote">Remote instance</MenuItem>
          </TextField>
          {local ? (
            <>
              <TextField
                label="Path"
                value={form.path}
                onChange={(e) => set({ path: e.target.value })}
                helperText="Root directory backups are written under."
                required
                slotProps={{ htmlInput: { spellCheck: false } }}
              />
              <TextField
                label="Retention"
                value={form.retention}
                onChange={(e) => set({ retention: e.target.value })}
                helperText='Delete objects older than this, e.g. "7d" or "168h". Empty keeps them forever.'
              />
            </>
          ) : (
            <TextField
              label="Endpoint"
              value={form.endpoint}
              onChange={(e) => set({ endpoint: e.target.value })}
              helperText="e.g. https://backup2.example.com:8443. This instance signs requests with its own identity."
              required
              slotProps={{ htmlInput: { spellCheck: false } }}
            />
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel}>Cancel</Button>
        <Button variant="contained" disabled={saving || !canSave} onClick={onSave}>
          {editing ? "Save" : "Create"}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function ServerConfigSection() {
  const [data, setData] = useState<ServerConfigListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);

  // dialog is null while closed; editing is the server being edited, or
  // null when creating a new one.
  const [dialog, setDialog] = useState<{ editing: ServerConfigJSON | null } | null>(null);
  const [form, setForm] = useState<ServerDefinitionJSON>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ServerConfigJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<ServerConfigListJSON>("/api/server-configs")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openDialog(editing: ServerConfigJSON | null) {
    setForm(editing ? { ...EMPTY_FORM, ...definitionOf(editing) } : EMPTY_FORM);
    setSaveError(null);
    setDialog({ editing });
  }

  async function save() {
    if (!dialog) return;
    const editing = dialog.editing;

    setSaving(true);
    setSaveError(null);
    try {
      await apiFetchOK(
        editing ? "/api/server-configs/" + encodeURIComponent(editing.name) : "/api/server-configs",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(definitionOf(form)),
        },
        "saving server failed",
      );
      setDialog(null);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  function remove(s: ServerConfigJSON) {
    apiFetchOK(
      "/api/server-configs/" + encodeURIComponent(s.name),
      { method: "DELETE" },
      "deleting server failed",
    )
      .then(() => refresh())
      .catch((err: unknown) => setError(errorText(err)));
  }

  const servers = data?.servers ?? [];
  const editing = data?.editing ?? false;

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          Upload destinations jobs' targets name. A change applies to every job using the server
          from its next run on.
        </Typography>
        {editing ? (
          <Button
            variant="contained"
            startIcon={<AddIcon />}
            onClick={() => openDialog(null)}
            sx={{ flexShrink: 0 }}
          >
            New server
          </Button>
        ) : null}
      </Stack>

      {data && !editing ? <EditingDisabledNotice /> : null}
      {error ? <Alert severity="error">{error}</Alert> : null}

      {data && !servers.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no servers configured yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Name</TableCell>
                <TableCell>Type</TableCell>
                <TableCell>Destination</TableCell>
                <TableCell>Retention</TableCell>
                <TableCell>Used by</TableCell>
                {editing ? <TableCell>Updated</TableCell> : null}
                {editing ? <TableCell align="right" /> : null}
              </TableRow>
            </TableHead>
            <TableBody>
              {servers.map((s) => (
                <TableRow key={s.name}>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {s.name}
                    {s.error ? (
                      <Tooltip title={s.error}>
                        <span>
                          <StatusChip state="failed" label="inactive" />
                        </span>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                  <TableCell>{s.type}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    <code>{s.type === "remote" ? s.endpoint : s.path}</code>
                  </TableCell>
                  <TableCell>{s.type === "remote" ? "—" : s.retention || "forever"}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {s.used_by.length ? s.used_by.join(", ") : "—"}
                  </TableCell>
                  {editing ? (
                    <TableCell>
                      {fmtTime(s.updated_at)}
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ display: "block" }}
                      >
                        by {s.updated_by}
                      </Typography>
                    </TableCell>
                  ) : null}
                  {editing ? (
                    <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                      <Tooltip title="Edit">
                        <IconButton size="small" onClick={() => openDialog(s)}>
                          <EditIcon fontSize="small" />
                        </IconButton>
                      </Tooltip>
                      <Tooltip title={s.used_by.length ? "In use" : "Delete"}>
                        <span>
                          <IconButton
                            size="small"
                            color="error"
                            disabled={s.used_by.length > 0}
                            onClick={() => setPendingDelete(s)}
                          >
                            <DeleteIcon fontSize="small" />
                          </IconButton>
                        </span>
                      </Tooltip>
                    </TableCell>
                  ) : null}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}

      {dialog ? (
        <ServerDialog
          editing={dialog.editing}
          form={form}
          saving={saving}
          error={saveError}
          onChange={setForm}
          onCancel={() => setDialog(null)}
          onSave={() => void save()}
        />
      ) : null}

      <ConfirmDialog
        open={pendingDelete !== null}
        message={
          <>
            Delete the server <strong>{pendingDelete?.name}</strong>? Backups already written to it
            are kept.
          </>
        }
        confirmLabel="Delete"
        onConfirm={() => {
          if (pendingDelete) remove(pendingDelete);
          setPendingDelete(null);
        }}
        onCancel={() => setPendingDelete(null)}
      />
    </Stack>
  );
}
