import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { NotificationConfigJSON, NotificationConfigListJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { fmtTime } from "../lib/format";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import Divider from "@mui/material/Divider";
import FormControlLabel from "@mui/material/FormControlLabel";
import IconButton from "@mui/material/IconButton";
import MenuItem from "@mui/material/MenuItem";
import Paper from "@mui/material/Paper";
import Stack from "@mui/material/Stack";
import Switch from "@mui/material/Switch";
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
import CloseIcon from "@mui/icons-material/Close";
import DeleteIcon from "@mui/icons-material/Delete";
import EditIcon from "@mui/icons-material/Edit";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";

const METHODS = ["POST", "PUT", "PATCH", "GET"];

// PLACEHOLDERS lists what each trigger substitutes into a webhook body or
// email subject/body; {server_name} works everywhere.
const PLACEHOLDERS: { trigger: string; names: string }[] = [
  { trigger: "Everywhere", names: "{server_name}" },
  { trigger: "Job failure", names: "{job} {error} {state} {duration} {started_at}" },
  { trigger: "Stale receiver", names: "{receiver_id} {path} {stale_after} {last_received}" },
  { trigger: "Download", names: "{user} {file} {receiver} {time}" },
  { trigger: "Report", names: "{start} {end} {receivers} {errors} {stale} {jobs} {job-errors}" },
];

// HeaderRow is one webhook header in the form. stored marks a header the
// server already holds a value for under originalName: its value is
// write-only, so an empty value field keeps it (sent as null).
interface HeaderRow {
  key: number;
  name: string;
  value: string;
  originalName: string | null;
}

interface NotificationForm {
  id: string;
  webhookOn: boolean;
  url: string;
  method: string;
  headers: HeaderRow[];
  webhookBody: string;
  emailOn: boolean;
  to: string;
  from: string;
  subject: string;
  emailBody: string;
  encryptOn: boolean;
  encryptRecipients: string;
}

let nextHeaderKey = 0;

function emptyForm(): NotificationForm {
  return {
    id: "",
    webhookOn: true,
    url: "",
    method: "POST",
    headers: [],
    webhookBody: "",
    emailOn: false,
    to: "",
    from: "",
    subject: "",
    emailBody: "",
    encryptOn: false,
    encryptRecipients: "",
  };
}

function formFrom(n: NotificationConfigJSON): NotificationForm {
  const f = emptyForm();
  f.id = n.id;
  f.webhookOn = !!n.webhook;
  if (n.webhook) {
    f.url = n.webhook.url;
    f.method = n.webhook.method || "POST";
    f.headers = Object.keys(n.webhook.headers || {})
      .sort()
      .map((name) => ({ key: nextHeaderKey++, name, value: "", originalName: name }));
    f.webhookBody = n.webhook.body;
  }
  f.emailOn = !!n.email;
  if (n.email) {
    f.to = n.email.to.join(", ");
    f.from = n.email.from;
    f.subject = n.email.subject;
    f.emailBody = n.email.body;
    f.encryptOn = !!n.email.encrypt;
    f.encryptRecipients = n.email.encrypt?.recipients.join(", ") ?? "";
  }
  return f;
}

function splitList(s: string): string[] {
  return s
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);
}

// requestBody builds the POST/PUT body: a kept stored header (unchanged
// name, empty value) is sent as null.
function requestBody(f: NotificationForm) {
  const headers: Record<string, string | null> = {};
  for (const h of f.headers) {
    const name = h.name.trim();
    if (!name) continue;
    headers[name] = h.originalName === name && h.value === "" ? null : h.value;
  }

  return {
    id: f.id.trim(),
    webhook: f.webhookOn
      ? { url: f.url.trim(), method: f.method, headers, body: f.webhookBody }
      : null,
    email: f.emailOn
      ? {
          to: splitList(f.to),
          from: f.from.trim(),
          subject: f.subject,
          body: f.emailBody,
          encrypt: f.encryptOn ? { recipients: splitList(f.encryptRecipients) } : null,
        }
      : null,
  };
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function HeadersEditor({
  rows,
  onChange,
}: {
  rows: HeaderRow[];
  onChange: (rows: HeaderRow[]) => void;
}) {
  const update = (key: number, patch: Partial<HeaderRow>) =>
    onChange(rows.map((r) => (r.key === key ? { ...r, ...patch } : r)));

  return (
    <Stack spacing={1}>
      <Typography variant="body2" color="text.secondary">
        Headers — values are never shown again once saved; leave a saved value empty to keep it.
      </Typography>
      {rows.map((r) => {
        const kept = r.originalName !== null && r.originalName === r.name.trim();
        return (
          <Stack key={r.key} direction="row" spacing={1} sx={{ alignItems: "center" }}>
            <TextField
              size="small"
              label="Name"
              value={r.name}
              onChange={(e) => update(r.key, { name: e.target.value })}
              sx={{ flex: 1 }}
              slotProps={{ htmlInput: { spellCheck: false } }}
            />
            <TextField
              size="small"
              label="Value"
              value={r.value}
              onChange={(e) => update(r.key, { value: e.target.value })}
              placeholder={kept ? "•••••• (unchanged)" : ""}
              type="password"
              autoComplete="new-password"
              sx={{ flex: 2 }}
            />
            <IconButton
              size="small"
              aria-label="Remove header"
              onClick={() => onChange(rows.filter((x) => x.key !== r.key))}
            >
              <CloseIcon fontSize="small" />
            </IconButton>
          </Stack>
        );
      })}
      <Button
        size="small"
        startIcon={<AddIcon />}
        sx={{ alignSelf: "flex-start" }}
        onClick={() =>
          onChange([...rows, { key: nextHeaderKey++, name: "", value: "", originalName: null }])
        }
      >
        Add header
      </Button>
    </Stack>
  );
}

function PlaceholderHelp() {
  return (
    <Accordion disableGutters variant="outlined">
      <AccordionSummary expandIcon={<ExpandMoreIcon />}>
        <Typography variant="body2">Placeholders for bodies and subjects</Typography>
      </AccordionSummary>
      <AccordionDetails>
        <Stack spacing={0.5}>
          {PLACEHOLDERS.map((p) => (
            <Typography key={p.trigger} variant="body2" sx={{ overflowWrap: "anywhere" }}>
              <strong>{p.trigger}:</strong> <code>{p.names}</code>
            </Typography>
          ))}
          <Typography variant="caption" color="text.secondary">
            Leave a body or subject empty to use the trigger&apos;s default text.
          </Typography>
        </Stack>
      </AccordionDetails>
    </Accordion>
  );
}

function NotificationDialog({
  editing,
  form,
  smtpConfigured,
  saving,
  error,
  onChange,
  onCancel,
  onSave,
}: {
  editing: NotificationConfigJSON | null;
  form: NotificationForm;
  smtpConfigured: boolean;
  saving: boolean;
  error: string | null;
  onChange: (f: NotificationForm) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<NotificationForm>) => onChange({ ...form, ...patch });
  const canSave =
    !!form.id.trim() &&
    (form.webhookOn || form.emailOn) &&
    (!form.webhookOn || !!form.url.trim()) &&
    (!form.emailOn || splitList(form.to).length > 0);

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="md">
      <DialogTitle>{editing ? `Edit notification ${editing.id}` : "New notification"}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <TextField
            label="ID"
            value={form.id}
            onChange={(e) => set({ id: e.target.value })}
            helperText="How jobs, receivers, and the report refer to it. Can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
          />

          <Divider />
          <FormControlLabel
            control={
              <Switch
                checked={form.webhookOn}
                onChange={(e) => set({ webhookOn: e.target.checked })}
              />
            }
            label="Webhook"
          />
          {form.webhookOn ? (
            <Stack spacing={2}>
              <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
                <TextField
                  select
                  label="Method"
                  value={form.method}
                  onChange={(e) => set({ method: e.target.value })}
                  sx={{ minWidth: 120 }}
                >
                  {METHODS.map((m) => (
                    <MenuItem key={m} value={m}>
                      {m}
                    </MenuItem>
                  ))}
                </TextField>
                <TextField
                  label="URL"
                  value={form.url}
                  onChange={(e) => set({ url: e.target.value })}
                  placeholder="https://hooks.example.com/…"
                  required
                  fullWidth
                  slotProps={{ htmlInput: { spellCheck: false } }}
                />
              </Stack>
              <HeadersEditor rows={form.headers} onChange={(headers) => set({ headers })} />
              <TextField
                label="Body"
                value={form.webhookBody}
                onChange={(e) => set({ webhookBody: e.target.value })}
                helperText="Empty sends the trigger's default JSON summary."
                multiline
                minRows={3}
                slotProps={{
                  htmlInput: { spellCheck: false, style: { fontFamily: "monospace" } },
                }}
              />
            </Stack>
          ) : null}

          <Divider />
          <FormControlLabel
            control={
              <Switch
                checked={form.emailOn}
                onChange={(e) => set({ emailOn: e.target.checked })}
                disabled={!smtpConfigured && !form.emailOn}
              />
            }
            label="Email"
          />
          {!smtpConfigured ? (
            <Alert severity="info">
              Email needs <code>smtp:</code> in the config file. The SMTP server isn&apos;t
              configurable here.
            </Alert>
          ) : null}
          {form.emailOn ? (
            <Stack spacing={2}>
              <TextField
                label="To"
                value={form.to}
                onChange={(e) => set({ to: e.target.value })}
                helperText="Comma-separated addresses."
                required
              />
              <TextField
                label="From"
                value={form.from}
                onChange={(e) => set({ from: e.target.value })}
                helperText="Empty uses the SMTP username."
              />
              <TextField
                label="Subject"
                value={form.subject}
                onChange={(e) => set({ subject: e.target.value })}
              />
              <TextField
                label="Body"
                value={form.emailBody}
                onChange={(e) => set({ emailBody: e.target.value })}
                multiline
                minRows={3}
              />
              <FormControlLabel
                control={
                  <Switch
                    checked={form.encryptOn}
                    onChange={(e) => set({ encryptOn: e.target.checked })}
                  />
                }
                label="Encrypt body with GPG"
              />
              {form.encryptOn ? (
                <TextField
                  label="GPG recipients"
                  value={form.encryptRecipients}
                  onChange={(e) => set({ encryptRecipients: e.target.value })}
                  helperText="Comma-separated fingerprints or addresses; their keys must be in this instance's keyring."
                  required
                />
              ) : null}
            </Stack>
          ) : null}

          <PlaceholderHelp />
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

function channelSummary(n: NotificationConfigJSON) {
  const chips = [];
  if (n.webhook) {
    let host = n.webhook.url;
    try {
      host = new URL(n.webhook.url).host;
    } catch {
      /* keep the raw url */
    }
    chips.push(
      <Chip key="webhook" size="small" variant="outlined" label={`${n.webhook.method} ${host}`} />,
    );
  }
  if (n.email) {
    chips.push(
      <Chip
        key="email"
        size="small"
        variant="outlined"
        label={`email ${n.email.to.join(", ")}${n.email.encrypt ? " (encrypted)" : ""}`}
      />,
    );
  }
  return (
    <Stack direction="row" spacing={0.5} useFlexGap sx={{ flexWrap: "wrap" }}>
      {chips}
    </Stack>
  );
}

export function NotificationConfigSection() {
  const [data, setData] = useState<NotificationConfigListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [dialog, setDialog] = useState<{ editing: NotificationConfigJSON | null } | null>(null);
  const [form, setForm] = useState<NotificationForm>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<NotificationConfigJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<NotificationConfigListJSON>("/api/notification-configs")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openDialog(editing: NotificationConfigJSON | null) {
    setForm(editing ? formFrom(editing) : emptyForm());
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
          ? "/api/notification-configs/" + encodeURIComponent(editing.id)
          : "/api/notification-configs",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(requestBody(form)),
        },
        "saving notification failed",
      );
      setDialog(null);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  function remove(n: NotificationConfigJSON) {
    apiFetchOK(
      "/api/notification-configs/" + encodeURIComponent(n.id),
      { method: "DELETE" },
      "deleting notification failed",
    )
      .then(() => refresh())
      .catch((err: unknown) => setError(errorText(err)));
  }

  const notifications = data?.notifications ?? [];

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          Changes apply immediately. A notification can&apos;t be deleted while something uses it.
        </Typography>
        <Button
          variant="contained"
          startIcon={<AddIcon />}
          onClick={() => openDialog(null)}
          disabled={!data}
          sx={{ flexShrink: 0 }}
        >
          New notification
        </Button>
      </Stack>

      {error ? <Alert severity="error">{error}</Alert> : null}

      {data && !notifications.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no notifications configured yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>ID</TableCell>
                <TableCell>Channels</TableCell>
                <TableCell>Used by</TableCell>
                <TableCell>Updated</TableCell>
                <TableCell align="right" />
              </TableRow>
            </TableHead>
            <TableBody>
              {notifications.map((n) => (
                <TableRow key={n.id}>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {n.id}
                    {n.error ? (
                      <Tooltip title={n.error}>
                        <span>
                          <StatusChip state="failed" label="inactive" />
                        </span>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                  <TableCell>{channelSummary(n)}</TableCell>
                  <TableCell>
                    {n.used_by.length ? (
                      n.used_by.map((u) => (
                        <Typography key={u} variant="caption" sx={{ display: "block" }}>
                          {u}
                        </Typography>
                      ))
                    ) : (
                      <Typography variant="caption" color="text.secondary">
                        unused
                      </Typography>
                    )}
                  </TableCell>
                  <TableCell>
                    {fmtTime(n.updated_at)}
                    <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
                      by {n.updated_by}
                    </Typography>
                  </TableCell>
                  <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                    <Tooltip title="Edit">
                      <IconButton size="small" onClick={() => openDialog(n)}>
                        <EditIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                    <Tooltip
                      title={n.used_by.length ? "In use — remove its references first" : "Delete"}
                    >
                      <span>
                        <IconButton
                          size="small"
                          color="error"
                          disabled={n.used_by.length > 0}
                          onClick={() => setPendingDelete(n)}
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
        <NotificationDialog
          editing={dialog.editing}
          form={form}
          smtpConfigured={data?.smtp_configured ?? false}
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
            Delete the notification <strong>{pendingDelete?.id}</strong>?
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
