import { useCallback, useEffect, useRef, useState, type ChangeEvent } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { TrustedServerJSON, TrustedServerListJSON } from "../api/types";
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
import UploadFileIcon from "@mui/icons-material/UploadFile";

// ServerForm is the create/edit dialog's state, as sent to POST/PUT
// /api/trusted-servers.
interface ServerForm {
  id: string;
  name: string;
  public_key: string;
}

const EMPTY_FORM: ServerForm = { id: "", name: "", public_key: "" };

const MONO = { fontFamily: "monospace", fontSize: ".8rem" };

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// parseIdentityFile reads an identity file exported from another instance's
// Identity page (see IdentitySection's exportIdentity), throwing when it
// isn't one.
function parseIdentityFile(text: string): ServerForm {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    throw new Error("The file isn't valid JSON.");
  }

  const d = data as Partial<Record<keyof ServerForm, unknown>>;
  if (
    typeof d !== "object" ||
    d === null ||
    typeof d.id !== "string" ||
    typeof d.public_key !== "string" ||
    !d.id.trim() ||
    !d.public_key.trim()
  ) {
    throw new Error("The file isn't an exported server identity: it needs an id and public_key.");
  }

  return { id: d.id, name: typeof d.name === "string" ? d.name : "", public_key: d.public_key };
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
  editing: TrustedServerJSON | null;
  form: ServerForm;
  saving: boolean;
  error: string | null;
  onChange: (f: ServerForm) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<ServerForm>) => onChange({ ...form, ...patch });
  const canSave = !!form.id.trim() && !!form.name.trim() && !!form.public_key.trim();
  const fileInput = useRef<HTMLInputElement>(null);
  const [importError, setImportError] = useState<string | null>(null);

  function importFile(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    // Reset so picking the same file again still fires a change.
    e.target.value = "";
    if (!file) return;

    file
      .text()
      .then((text) => {
        const imported = parseIdentityFile(text);
        // An existing server's ID can't change, so its file must match.
        if (editing && imported.id !== editing.id) {
          throw new Error(
            `The file is for server ${imported.id}, not ${editing.id}. Create a new trusted server for it instead.`,
          );
        }
        // Keep a name already typed when the file has none.
        onChange({ ...imported, name: imported.name || form.name });
        setImportError(null);
      })
      .catch((err: unknown) => setImportError(errorText(err)));
  }

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="sm">
      <DialogTitle>
        {editing ? `Edit trusted server ${editing.name}` : "New trusted server"}
      </DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <Stack direction="row" spacing={1.5} sx={{ alignItems: "center" }}>
            <Button
              size="small"
              variant="outlined"
              startIcon={<UploadFileIcon />}
              onClick={() => fileInput.current?.click()}
            >
              Import from file
            </Button>
            <Typography variant="caption" color="text.secondary">
              An identity file exported from the sending instance's Identity page.
            </Typography>
            <input
              ref={fileInput}
              type="file"
              accept=".json,application/json"
              hidden
              onChange={importFile}
            />
          </Stack>
          {importError ? <Alert severity="error">{importError}</Alert> : null}
          <TextField
            label="Server ID"
            value={form.id}
            onChange={(e) => set({ id: e.target.value })}
            helperText="The sending instance's UUID, shown on its Identity page. Can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
            slotProps={{ htmlInput: { spellCheck: false, style: MONO } }}
          />
          <TextField
            label="Name"
            value={form.name}
            onChange={(e) => set({ name: e.target.value })}
            helperText="A label to tell servers apart, e.g. its hostname."
            autoFocus={!!editing}
            required
          />
          <TextField
            label="Public key"
            value={form.public_key}
            onChange={(e) => set({ public_key: e.target.value })}
            helperText="The sending instance's public key, shown on its Identity page. Replacing it takes effect immediately."
            placeholder={"-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----"}
            multiline
            minRows={4}
            required
            slotProps={{ htmlInput: { spellCheck: false, style: MONO } }}
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

export function TrustedServersSection() {
  const [data, setData] = useState<TrustedServerListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);

  // dialog is null while closed; editing is the server being edited, or
  // null when creating a new one.
  const [dialog, setDialog] = useState<{ editing: TrustedServerJSON | null } | null>(null);
  const [form, setForm] = useState<ServerForm>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<TrustedServerJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<TrustedServerListJSON>("/api/trusted-servers")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openDialog(editing: TrustedServerJSON | null) {
    setForm(
      editing ? { id: editing.id, name: editing.name, public_key: editing.public_key } : EMPTY_FORM,
    );
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
        editing ? "/api/trusted-servers/" + encodeURIComponent(editing.id) : "/api/trusted-servers",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ...form, id: form.id.trim() }),
        },
        "saving trusted server failed",
      );
      setDialog(null);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  function remove(s: TrustedServerJSON) {
    apiFetchOK(
      "/api/trusted-servers/" + encodeURIComponent(s.id),
      { method: "DELETE" },
      "deleting trusted server failed",
    )
      .then(() => refresh())
      .catch((err: unknown) => setError(errorText(err)));
  }

  const servers = data?.servers ?? [];

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          Instances allowed to send backups here. Allow each one on the receivers it may write to
          under Receiver settings. A server can't be deleted while a receiver allows it.
        </Typography>
        <Button
          variant="contained"
          startIcon={<AddIcon />}
          onClick={() => openDialog(null)}
          disabled={!data}
          sx={{ flexShrink: 0 }}
        >
          New trusted server
        </Button>
      </Stack>

      {error ? <Alert severity="error">{error}</Alert> : null}

      {data && !servers.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no trusted servers yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Name</TableCell>
                <TableCell>Server ID</TableCell>
                <TableCell>Key fingerprint</TableCell>
                <TableCell>Used by</TableCell>
                <TableCell>Updated</TableCell>
                <TableCell align="right" />
              </TableRow>
            </TableHead>
            <TableBody>
              {servers.map((s) => (
                <TableRow key={s.id}>
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
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    <code>{s.id}</code>
                  </TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    <code>{s.fingerprint || "—"}</code>
                  </TableCell>
                  <TableCell>
                    {s.used_by.length
                      ? s.used_by.map((u) => u.replace(/^receiver /, "")).join(", ")
                      : "—"}
                  </TableCell>
                  <TableCell>
                    {fmtTime(s.updated_at)}
                    <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
                      by {s.updated_by}
                    </Typography>
                  </TableCell>
                  <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                    <Tooltip title="Edit">
                      <IconButton size="small" onClick={() => openDialog(s)}>
                        <EditIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                    <Tooltip title={s.used_by.length ? "In use by a receiver" : "Delete"}>
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
            Delete the trusted server <strong>{pendingDelete?.name}</strong>? It can no longer be
            allowed on receivers until added again.
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
