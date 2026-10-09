import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { CommandConfigJSON, CommandConfigListJSON, CommandDefinitionJSON } from "../api/types";
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

const EMPTY_FORM: CommandDefinitionJSON = { id: "", cmd: "", timeout: "" };

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function CommandDialog({
  editing,
  form,
  saving,
  error,
  onChange,
  onCancel,
  onSave,
}: {
  editing: CommandConfigJSON | null;
  form: CommandDefinitionJSON;
  saving: boolean;
  error: string | null;
  onChange: (f: CommandDefinitionJSON) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<CommandDefinitionJSON>) => onChange({ ...form, ...patch });
  const canSave = !!form.id.trim() && !!form.cmd.trim();

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="sm">
      <DialogTitle>{editing ? `Edit command ${editing.id}` : "New command"}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <TextField
            label="ID"
            value={form.id}
            onChange={(e) => set({ id: e.target.value })}
            helperText="What a job target's on-error/on-recover names. Can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
          />
          <TextField
            label="Command"
            value={form.cmd}
            onChange={(e) => set({ cmd: e.target.value })}
            helperText="Run through the shell. Context comes in GBT_EVENT, GBT_JOB, GBT_TARGET, GBT_SERVER, GBT_ERROR, GBT_CONSECUTIVE_FAILURES, and GBT_TIME."
            required
            multiline
            minRows={2}
            slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: "monospace" } } }}
          />
          <TextField
            label="Timeout"
            value={form.timeout}
            onChange={(e) => set({ timeout: e.target.value })}
            helperText='How long one run may take, e.g. "15s". Empty means 30s.'
          />
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

export function CommandConfigSection() {
  const [data, setData] = useState<CommandConfigListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);

  // dialog is null while closed; editing is the command being edited, or
  // null when creating a new one.
  const [dialog, setDialog] = useState<{ editing: CommandConfigJSON | null } | null>(null);
  const [form, setForm] = useState<CommandDefinitionJSON>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<CommandConfigJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<CommandConfigListJSON>("/api/command-configs")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openDialog(editing: CommandConfigJSON | null) {
    setForm(editing ? { id: editing.id, cmd: editing.cmd, timeout: editing.timeout } : EMPTY_FORM);
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
        editing ? "/api/command-configs/" + encodeURIComponent(editing.id) : "/api/command-configs",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ...form, id: form.id.trim() }),
        },
        "saving command failed",
      );
      setDialog(null);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  function remove(c: CommandConfigJSON) {
    apiFetchOK(
      "/api/command-configs/" + encodeURIComponent(c.id),
      { method: "DELETE" },
      "deleting command failed",
    )
      .then(() => refresh())
      .catch((err: unknown) => setError(errorText(err)));
  }

  const commands = data?.commands ?? [];
  const editing = data?.editing ?? false;

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          Shell commands a job's targets run after repeated failures (on-error) or once they recover
          (on-recover).
        </Typography>
        {editing ? (
          <Button
            variant="contained"
            startIcon={<AddIcon />}
            onClick={() => openDialog(null)}
            sx={{ flexShrink: 0 }}
          >
            New command
          </Button>
        ) : null}
      </Stack>

      {data && !editing ? <EditingDisabledNotice /> : null}
      {error ? <Alert severity="error">{error}</Alert> : null}

      {data && !commands.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no commands configured yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>ID</TableCell>
                <TableCell>Command</TableCell>
                <TableCell>Timeout</TableCell>
                <TableCell>Used by</TableCell>
                {editing ? <TableCell>Updated</TableCell> : null}
                {editing ? <TableCell align="right" /> : null}
              </TableRow>
            </TableHead>
            <TableBody>
              {commands.map((c) => (
                <TableRow key={c.id}>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {c.id}
                    {c.error ? (
                      <Tooltip title={c.error}>
                        <span>
                          <StatusChip state="failed" label="inactive" />
                        </span>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    <code>{c.cmd}</code>
                  </TableCell>
                  <TableCell>{c.timeout || "30s"}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {c.used_by.length ? c.used_by.join(", ") : "—"}
                  </TableCell>
                  {editing ? (
                    <TableCell>
                      {fmtTime(c.updated_at)}
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ display: "block" }}
                      >
                        by {c.updated_by}
                      </Typography>
                    </TableCell>
                  ) : null}
                  {editing ? (
                    <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                      <Tooltip title="Edit">
                        <IconButton size="small" onClick={() => openDialog(c)}>
                          <EditIcon fontSize="small" />
                        </IconButton>
                      </Tooltip>
                      <Tooltip title={c.used_by.length ? "In use" : "Delete"}>
                        <span>
                          <IconButton
                            size="small"
                            color="error"
                            disabled={c.used_by.length > 0}
                            onClick={() => setPendingDelete(c)}
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
        <CommandDialog
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
            Delete the command <strong>{pendingDelete?.id}</strong>?
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
