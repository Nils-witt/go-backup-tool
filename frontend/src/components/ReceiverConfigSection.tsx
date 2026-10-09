import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type {
  ReceiverConfigJSON,
  ReceiverConfigListJSON,
  TrustedServerOptionJSON,
} from "../api/types";
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
import InputAdornment from "@mui/material/InputAdornment";
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
// fields, as sent to POST/PUT /api/receiver-configs — except path, which is
// relative to the base dir here (see joinPath).
interface ReceiverForm {
  id: string;
  allowed_servers: string[];
  public_key: string;
  path: string;
  retention: string;
  stale_after: string;
  stale_notifications: string[];
  download_notifications: string[];
}

const EMPTY_FORM: ReceiverForm = {
  id: "",
  allowed_servers: [],
  public_key: "",
  path: "",
  retention: "",
  stale_after: "",
  stale_notifications: [],
  download_notifications: [],
};

// basePrefix is baseDir with exactly one trailing slash.
function basePrefix(baseDir: string): string {
  return baseDir.replace(/\/+$/, "") + "/";
}

// relativePath returns path relative to baseDir, or null when it isn't
// inside it (e.g. a receiver imported from the config file).
function relativePath(baseDir: string, path: string): string | null {
  const prefix = basePrefix(baseDir);
  return baseDir && path.startsWith(prefix) ? path.slice(prefix.length) : null;
}

// joinPath turns the dialog's relative path back into the absolute path the
// API expects. The server cleans it and rejects anything escaping baseDir.
function joinPath(baseDir: string, rel: string): string {
  return basePrefix(baseDir) + rel.trim().replace(/^\/+/, "");
}

function formFrom(r: ReceiverConfigJSON, baseDir: string): ReceiverForm {
  return {
    id: r.id,
    allowed_servers: r.allowed_servers,
    public_key: r.public_key,
    path: relativePath(baseDir, r.path) ?? "",
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
      helperText={
        options.length ? helperText : "No notifications yet — create one on the Notifications page."
      }
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

// serverLabel shows a trusted server by name, falling back to its id when
// it's no longer in the list.
function serverLabel(servers: TrustedServerOptionJSON[], id: string): string {
  return servers.find((s) => s.id === id)?.name ?? id;
}

function ReceiverDialog({
  editing,
  form,
  baseDir,
  notifications,
  trustedServers,
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
  trustedServers: TrustedServerOptionJSON[];
  saving: boolean;
  error: string | null;
  onChange: (f: ReceiverForm) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<ReceiverForm>) => onChange({ ...form, ...patch });
  // outsidePath is the edited receiver's stored path when it isn't inside
  // baseDir; leaving the path empty keeps it.
  const outsidePath = editing && relativePath(baseDir, editing.path) === null ? editing.path : null;
  const canSave =
    !!form.id.trim() &&
    (form.allowed_servers.length > 0 || !!form.public_key.trim()) &&
    (!!form.path.trim() || outsidePath !== null);

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
            select
            label="Allowed servers"
            value={form.allowed_servers}
            onChange={(e) => {
              const v = e.target.value as unknown as string | string[];
              set({ allowed_servers: typeof v === "string" ? v.split(",") : v });
            }}
            helperText={
              trustedServers.length
                ? "Trusted servers that may send backups to this receiver."
                : "No trusted servers yet — add the sending instance on the Trusted servers page."
            }
            disabled={!trustedServers.length}
            required={!form.public_key.trim()}
            slotProps={{
              select: {
                multiple: true,
                renderValue: (v) =>
                  (v as string[]).map((id) => serverLabel(trustedServers, id)).join(", "),
              },
            }}
          >
            {trustedServers.map((s) => (
              <MenuItem key={s.id} value={s.id}>
                {s.name}
                <Typography variant="caption" color="text.secondary" sx={{ ml: 1 }}>
                  {s.id}
                </Typography>
              </MenuItem>
            ))}
          </TextField>
          {form.public_key.trim() ? (
            <Alert
              severity="warning"
              action={
                <Button color="inherit" size="small" onClick={() => set({ public_key: "" })}>
                  Remove
                </Button>
              }
            >
              This receiver still accepts the deprecated single sender public key. Add the sender as
              a trusted server, allow it above, then remove the old key.
            </Alert>
          ) : null}
          <TextField
            label="Path"
            value={form.path}
            onChange={(e) => set({ path: e.target.value })}
            helperText={
              outsidePath !== null
                ? `Currently ${outsidePath}, outside the base dir. Leave empty to keep it.`
                : "Directory incoming objects are written to, inside the base dir."
            }
            required={outsidePath === null}
            disabled={!baseDir}
            slotProps={{
              htmlInput: { spellCheck: false },
              input: {
                startAdornment: baseDir ? (
                  <InputAdornment position="start">
                    <code>{basePrefix(baseDir)}</code>
                  </InputAdornment>
                ) : undefined,
              },
            }}
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
    setForm(editing ? formFrom(editing, baseDir) : EMPTY_FORM);
    setSaveError(null);
    setDialog({ editing });
  }

  async function save() {
    if (!dialog) return;
    const editing = dialog.editing;
    // An empty path while editing keeps a stored path outside the base dir
    // (the dialog only allows that case).
    const path = form.path.trim() ? joinPath(baseDir, form.path) : (editing?.path ?? "");

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
          body: JSON.stringify({ ...form, id: form.id.trim(), path }),
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
  const trustedServers = data?.trusted_servers ?? [];

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
                <TableCell>Allowed servers</TableCell>
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
                    {r.allowed_servers.map((id) => serverLabel(trustedServers, id)).join(", ")}
                    {r.public_key ? (
                      <Typography variant="caption" color="warning.main" sx={{ display: "block" }}>
                        + legacy public key
                      </Typography>
                    ) : null}
                    {!r.allowed_servers.length && !r.public_key ? "—" : null}
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
          trustedServers={trustedServers}
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
