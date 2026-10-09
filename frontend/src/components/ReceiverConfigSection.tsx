import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { ReceiverConfigJSON, ReceiverConfigListJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
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

// ReceiverForm is the create/edit dialog's state: a receiver's editable
// fields, as sent to POST/PUT /api/receiver-configs.
interface ReceiverForm {
  id: string;
  public_key: string;
  path: string;
  retention: string;
  stale_after: string;
  stale_notifications: string[];
  download_notifications: string[];
}

const EMPTY_FORM: ReceiverForm = {
  id: "",
  public_key: "",
  path: "",
  retention: "",
  stale_after: "",
  stale_notifications: [],
  download_notifications: [],
};

function formFrom(r: ReceiverConfigJSON): ReceiverForm {
  return {
    id: r.id,
    public_key: r.public_key,
    path: r.path,
    retention: r.retention,
    stale_after: r.stale_after,
    stale_notifications: r.stale_notifications,
    download_notifications: r.download_notifications,
  };
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function NotificationSelect({
  label,
  helperText,
  options,
  value,
  onChange,
}: {
  label: string;
  helperText: string;
  options: string[];
  value: string[];
  onChange: (v: string[]) => void;
}) {
  return (
    <TextField
      select
      label={label}
      value={value}
      onChange={(e) => {
        const v = e.target.value as unknown as string | string[];
        onChange(typeof v === "string" ? v.split(",") : v);
      }}
      helperText={options.length ? helperText : "No notifications yet — create one on the Notifications page."}
      disabled={!options.length}
      slotProps={{ select: { multiple: true } }}
    >
      {options.map((id) => (
        <MenuItem key={id} value={id}>
          {id}
        </MenuItem>
      ))}
    </TextField>
  );
}

function ReceiverDialog({
  editing,
  form,
  baseDir,
  notifications,
  saving,
  error,
  onChange,
  onCancel,
  onSave,
}: {
  editing: ReceiverConfigJSON | null;
  form: ReceiverForm;
  baseDir: string;
  notifications: string[];
  saving: boolean;
  error: string | null;
  onChange: (f: ReceiverForm) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<ReceiverForm>) => onChange({ ...form, ...patch });
  const canSave = !!form.id.trim() && !!form.public_key.trim() && !!form.path.trim();

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="sm">
      <DialogTitle>{editing ? `Edit receiver ${editing.id}` : "New receiver"}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <TextField
            label="ID"
            value={form.id}
            onChange={(e) => set({ id: e.target.value })}
            helperText="What the sending instance's target names as its bucket. Can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
          />
          <TextField
            label="Sender public key"
            value={form.public_key}
            onChange={(e) => set({ public_key: e.target.value })}
            helperText="The sending instance's server.pub, shown on its Identity page."
            placeholder={"-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----"}
            multiline
            minRows={4}
            required
            slotProps={{
              htmlInput: {
                spellCheck: false,
                style: { fontFamily: "monospace", fontSize: ".8rem" },
              },
            }}
          />
          <TextField
            label="Path"
            value={form.path}
            onChange={(e) => set({ path: e.target.value })}
            helperText={
              baseDir
                ? `Directory incoming objects are written to; must be inside ${baseDir}.`
                : "Directory incoming objects are written to."
            }
            required
            slotProps={{ htmlInput: { spellCheck: false } }}
          />
          <TextField
            label="Retention"
            value={form.retention}
            onChange={(e) => set({ retention: e.target.value })}
            helperText='Delete objects older than this, e.g. "30d" or "12h". Empty keeps them forever.'
          />
          <TextField
            label="Stale after"
            value={form.stale_after}
            onChange={(e) => set({ stale_after: e.target.value })}
            helperText='Alert when the newest file is older than this, e.g. "26h". Needs stale notifications.'
          />
          <NotificationSelect
            label="Stale notifications"
            helperText="Fired once per gap when the receiver turns stale."
            options={notifications}
            value={form.stale_notifications}
            onChange={(v) => set({ stale_notifications: v })}
          />
          <NotificationSelect
            label="Download notifications"
            helperText="Fired every time a file is downloaded from this receiver."
            options={notifications}
            value={form.download_notifications}
            onChange={(v) => set({ download_notifications: v })}
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

export function ReceiverConfigSection() {
  const [data, setData] = useState<ReceiverConfigListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);

  // dialog is null while closed; editing is the receiver being edited, or
  // null when creating a new one.
  const [dialog, setDialog] = useState<{ editing: ReceiverConfigJSON | null } | null>(null);
  const [form, setForm] = useState<ReceiverForm>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ReceiverConfigJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<ReceiverConfigListJSON>("/api/receiver-configs")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openDialog(editing: ReceiverConfigJSON | null) {
    setForm(editing ? formFrom(editing) : EMPTY_FORM);
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
        editing
          ? "/api/receiver-configs/" + encodeURIComponent(editing.id)
          : "/api/receiver-configs",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ...form, id: form.id.trim() }),
        },
        "saving receiver failed",
      );
      setDialog(null);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  function remove(r: ReceiverConfigJSON) {
    apiFetchOK(
      "/api/receiver-configs/" + encodeURIComponent(r.id),
      { method: "DELETE" },
      "deleting receiver failed",
    )
      .then(() => refresh())
      .catch((err: unknown) => setError(errorText(err)));
  }

  const receivers = data?.receivers ?? [];
  const baseDir = data?.base_dir ?? "";

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          {baseDir ? (
            <>
              Receiver paths must be inside <code>{baseDir}</code>. Deleting a receiver keeps its
              files on disk.
            </>
          ) : (
            <>
              Set <code>webui.receivers-base-dir</code> in the config file to create receivers or
              change their paths here.
            </>
          )}
        </Typography>
        <Button
          variant="contained"
          startIcon={<AddIcon />}
          onClick={() => openDialog(null)}
          disabled={!data || !baseDir}
          sx={{ flexShrink: 0 }}
        >
          New receiver
        </Button>
      </Stack>

      {error ? <Alert severity="error">{error}</Alert> : null}

      {data && !receivers.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no receivers configured yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>ID</TableCell>
                <TableCell>Path</TableCell>
                <TableCell>Retention</TableCell>
                <TableCell>Stale after</TableCell>
                <TableCell>Notifications</TableCell>
                <TableCell>Updated</TableCell>
                <TableCell align="right" />
              </TableRow>
            </TableHead>
            <TableBody>
              {receivers.map((r) => (
                <TableRow key={r.id}>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {r.id}
                    {r.error ? (
                      <Tooltip title={r.error}>
                        <span>
                          <StatusChip state="failed" label="inactive" />
                        </span>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    <code>{r.path}</code>
                  </TableCell>
                  <TableCell>{r.retention || "forever"}</TableCell>
                  <TableCell>{r.stale_after || "—"}</TableCell>
                  <TableCell>
                    {r.stale_notifications.length ? (
                      <Typography variant="caption" sx={{ display: "block" }}>
                        stale: {r.stale_notifications.join(", ")}
                      </Typography>
                    ) : null}
                    {r.download_notifications.length ? (
                      <Typography variant="caption" sx={{ display: "block" }}>
                        download: {r.download_notifications.join(", ")}
                      </Typography>
                    ) : null}
                    {!r.stale_notifications.length && !r.download_notifications.length ? "—" : null}
                  </TableCell>
                  <TableCell>
                    {fmtTime(r.updated_at)}
                    <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
                      by {r.updated_by}
                    </Typography>
                  </TableCell>
                  <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                    <Tooltip title="Edit">
                      <IconButton size="small" onClick={() => openDialog(r)}>
                        <EditIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                    <Tooltip title="Delete">
                      <IconButton size="small" color="error" onClick={() => setPendingDelete(r)}>
                        <DeleteIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}

      {dialog ? (
        <ReceiverDialog
          editing={dialog.editing}
          form={form}
          baseDir={baseDir}
          notifications={data?.notifications ?? []}
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
            Delete the receiver <strong>{pendingDelete?.id}</strong>? Its sender is rejected from
            now on. Files already received stay on disk.
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
