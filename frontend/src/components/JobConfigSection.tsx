import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type {
  JobConfigJSON,
  JobConfigListJSON,
  JobDefinitionJSON,
  JobTargetJSON,
  ServerOptionJSON,
} from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { EditingDisabledNotice } from "./EditingDisabledNotice";
import { fmtTime } from "../lib/format";
import Accordion from "@mui/material/Accordion";
import AccordionDetails from "@mui/material/AccordionDetails";
import AccordionSummary from "@mui/material/AccordionSummary";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Checkbox from "@mui/material/Checkbox";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
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
import DeleteIcon from "@mui/icons-material/Delete";
import EditIcon from "@mui/icons-material/Edit";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";

// JobForm is the create/edit dialog's state: a job's definition, with its
// lists as editable text (one entry per line).
interface JobForm extends Omit<JobDefinitionJSON, "recipients"> {
  recipients: string;
}

const EMPTY_TARGET: JobTargetJSON = {
  server: "",
  bucket: "",
  retention: "",
  on_error: null,
  on_recover: null,
};

const EMPTY_FORM: JobForm = {
  name: "",
  command: "",
  key: "",
  targets: [EMPTY_TARGET],
  recipients: "",
  armor: false,
  gpg_bin: "",
  gpg_homedir: "",
  interval: "",
  start_time: "",
  staging_dir: "",
  failure_notifications: [],
};

function formFrom(j: JobConfigJSON): JobForm {
  return {
    name: j.name,
    command: j.command,
    key: j.key,
    targets: j.targets.length ? j.targets : [EMPTY_TARGET],
    recipients: j.recipients.join("\n"),
    armor: j.armor,
    gpg_bin: j.gpg_bin,
    gpg_homedir: j.gpg_homedir,
    interval: j.interval,
    start_time: j.start_time,
    staging_dir: j.staging_dir,
    failure_notifications: j.failure_notifications,
  };
}

function definitionOf(form: JobForm): JobDefinitionJSON {
  return {
    ...form,
    name: form.name.trim(),
    recipients: form.recipients
      .split(/[\n,]/)
      .map((r) => r.trim())
      .filter(Boolean),
  };
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function scheduleText(j: JobDefinitionJSON): string {
  if (!j.interval) return "once";
  return j.start_time ? `every ${j.interval} from ${fmtTime(j.start_time)}` : `every ${j.interval}`;
}

// CommandSelect picks one command id, or none ("") unless required.
function CommandSelect({
  label,
  commands,
  value,
  onChange,
  required,
  helperText,
}: {
  label: string;
  commands: string[];
  value: string;
  onChange: (v: string) => void;
  required?: boolean;
  helperText?: string;
}) {
  return (
    <TextField
      select
      size={required ? "medium" : "small"}
      label={label}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={!commands.length && !value}
      required={required}
      helperText={helperText}
      fullWidth={required}
      sx={{ minWidth: 160 }}
    >
      {required ? null : (
        <MenuItem value="">
          <em>none</em>
        </MenuItem>
      )}
      {commands.map((id) => (
        <MenuItem key={id} value={id}>
          {id}
        </MenuItem>
      ))}
      {value && !commands.includes(value) ? <MenuItem value={value}>{value}</MenuItem> : null}
    </TextField>
  );
}

function TargetEditor({
  target,
  servers,
  commands,
  onChange,
  onRemove,
}: {
  target: JobTargetJSON;
  servers: ServerOptionJSON[];
  commands: string[];
  onChange: (t: JobTargetJSON) => void;
  onRemove: (() => void) | null;
}) {
  const set = (patch: Partial<JobTargetJSON>) => onChange({ ...target, ...patch });
  const remote = servers.find((s) => s.name === target.server)?.type === "remote";

  return (
    <Paper variant="outlined" sx={{ p: 1.5 }}>
      <Stack spacing={1.5}>
        <Stack direction={{ xs: "column", sm: "row" }} spacing={1}>
          <TextField
            select
            size="small"
            label="Server"
            value={target.server}
            onChange={(e) => {
              // A retention override is only valid on a local server.
              const toRemote = servers.find((s) => s.name === e.target.value)?.type === "remote";
              set({ server: e.target.value, retention: toRemote ? "" : target.retention });
            }}
            required
            sx={{ minWidth: 160 }}
          >
            {servers.map((s) => (
              <MenuItem key={s.name} value={s.name}>
                {s.name}
                <Typography variant="caption" color="text.secondary" sx={{ ml: 1 }}>
                  {s.type}
                </Typography>
              </MenuItem>
            ))}
            {target.server && !servers.some((s) => s.name === target.server) ? (
              <MenuItem value={target.server}>{target.server}</MenuItem>
            ) : null}
          </TextField>
          <TextField
            size="small"
            label={remote ? "Receiver ID" : "Bucket"}
            value={target.bucket}
            onChange={(e) => set({ bucket: e.target.value })}
            required
            sx={{ flex: 1 }}
          />
          {!remote ? (
            <TextField
              size="small"
              label="Retention"
              value={target.retention}
              onChange={(e) => set({ retention: e.target.value })}
              placeholder="server's"
              sx={{ width: { sm: 120 } }}
            />
          ) : null}
          {onRemove ? (
            <Tooltip title="Remove target">
              <IconButton size="small" onClick={onRemove} sx={{ alignSelf: "center" }}>
                <DeleteIcon fontSize="small" />
              </IconButton>
            </Tooltip>
          ) : null}
        </Stack>
        <Stack
          direction={{ xs: "column", sm: "row" }}
          spacing={1}
          sx={{ alignItems: { sm: "center" } }}
        >
          <CommandSelect
            label="On error run"
            commands={commands}
            value={target.on_error?.command ?? ""}
            onChange={(command) =>
              set({
                on_error: command ? { after: 1, repeat: null, ...target.on_error, command } : null,
              })
            }
          />
          {target.on_error ? (
            <>
              <TextField
                size="small"
                type="number"
                label="After failures"
                value={target.on_error.after}
                onChange={(e) =>
                  set({ on_error: { ...target.on_error!, after: Number(e.target.value) } })
                }
                slotProps={{ htmlInput: { min: 1 } }}
                sx={{ width: 130 }}
              />
              <FormControlLabel
                control={
                  <Checkbox
                    size="small"
                    checked={target.on_error.repeat !== false}
                    onChange={(e) =>
                      set({
                        on_error: { ...target.on_error!, repeat: e.target.checked ? null : false },
                      })
                    }
                  />
                }
                label="Repeat"
              />
            </>
          ) : null}
          <CommandSelect
            label="On recover run"
            commands={commands}
            value={target.on_recover?.command ?? ""}
            onChange={(command) => set({ on_recover: command ? { command } : null })}
          />
        </Stack>
      </Stack>
    </Paper>
  );
}

function JobDialog({
  editing,
  form,
  data,
  saving,
  error,
  onChange,
  onCancel,
  onSave,
}: {
  editing: JobConfigJSON | null;
  form: JobForm;
  data: JobConfigListJSON;
  saving: boolean;
  error: string | null;
  onChange: (f: JobForm) => void;
  onCancel: () => void;
  onSave: () => void;
}) {
  const set = (patch: Partial<JobForm>) => onChange({ ...form, ...patch });
  const setTarget = (i: number, t: JobTargetJSON) =>
    set({ targets: form.targets.map((old, j) => (j === i ? t : old)) });
  const canSave =
    !!form.name.trim() &&
    !!form.command &&
    !!form.recipients.trim() &&
    form.targets.every((t) => t.server && t.bucket.trim());

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="md">
      <DialogTitle>{editing ? `Edit job ${editing.name}` : "New job"}</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <TextField
            label="Name"
            value={form.name}
            onChange={(e) => set({ name: e.target.value })}
            helperText="Its run history is kept under this name, so it can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
          />
          {!data.commands.length ? (
            <Alert severity="warning">No commands yet — create one on the Commands page.</Alert>
          ) : null}
          <CommandSelect
            label="Command"
            commands={data.commands}
            value={form.command}
            onChange={(command) => set({ command })}
            required
            helperText="Its standard output is the backup, encrypted before upload. Manage commands on the Commands page."
          />
          <TextField
            label="Key"
            value={form.key}
            onChange={(e) => set({ key: e.target.value })}
            placeholder="backup-{time}.gpg"
            helperText="Object name. {time} becomes the run's UTC timestamp, so repeats don't overwrite each other."
            slotProps={{ htmlInput: { spellCheck: false } }}
          />
          <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
            <TextField
              label="Interval"
              value={form.interval}
              onChange={(e) => set({ interval: e.target.value })}
              helperText='Repeat every, e.g. "6h". Empty runs once.'
              sx={{ flex: 1 }}
            />
            <TextField
              label="Start time"
              value={form.start_time}
              onChange={(e) => set({ start_time: e.target.value })}
              placeholder="2026-01-01T03:00:00Z"
              helperText="Anchors the interval grid (RFC 3339). Empty starts now."
              disabled={!form.interval.trim() && !form.start_time}
              sx={{ flex: 1 }}
            />
          </Stack>

          <Stack spacing={1}>
            <Typography variant="subtitle2">Targets</Typography>
            {!data.servers.length ? (
              <Alert severity="warning">No servers yet — create one on the Servers page.</Alert>
            ) : null}
            {form.targets.map((t, i) => (
              <TargetEditor
                key={i}
                target={t}
                servers={data.servers}
                commands={data.commands}
                onChange={(nt) => setTarget(i, nt)}
                onRemove={
                  form.targets.length > 1
                    ? () => set({ targets: form.targets.filter((_, j) => j !== i) })
                    : null
                }
              />
            ))}
            <Button
              startIcon={<AddIcon />}
              onClick={() => set({ targets: [...form.targets, EMPTY_TARGET] })}
              sx={{ alignSelf: "flex-start" }}
            >
              Add target
            </Button>
          </Stack>

          <TextField
            label="Recipients"
            value={form.recipients}
            onChange={(e) => set({ recipients: e.target.value })}
            helperText="GPG identities (fingerprint or email) to encrypt to, one per line. Their keys must be in the keyring."
            required
            multiline
            minRows={2}
            slotProps={{ htmlInput: { spellCheck: false } }}
          />
          <FormControlLabel
            control={
              <Switch checked={form.armor} onChange={(e) => set({ armor: e.target.checked })} />
            }
            label="ASCII-armored output"
          />
          <TextField
            select
            label="Failure notifications"
            value={form.failure_notifications}
            onChange={(e) => {
              const v = e.target.value as unknown as string | string[];
              set({ failure_notifications: typeof v === "string" ? v.split(",") : v });
            }}
            helperText={
              data.notifications.length
                ? "Fired whenever a run fails on any target."
                : "No notifications yet — create one on the Notifications page."
            }
            disabled={!data.notifications.length}
            slotProps={{ select: { multiple: true } }}
          >
            {data.notifications.map((id) => (
              <MenuItem key={id} value={id}>
                {id}
              </MenuItem>
            ))}
          </TextField>

          <Accordion variant="outlined" disableGutters>
            <AccordionSummary expandIcon={<ExpandMoreIcon />}>
              <Typography variant="body2">Advanced</Typography>
            </AccordionSummary>
            <AccordionDetails>
              <Stack spacing={2}>
                <TextField
                  label="Staging directory"
                  value={form.staging_dir}
                  onChange={(e) => set({ staging_dir: e.target.value })}
                  helperText="Where the encrypted backup is written before uploading. Empty uses the OS temp directory."
                />
                <TextField
                  label="GPG binary"
                  value={form.gpg_bin}
                  onChange={(e) => set({ gpg_bin: e.target.value })}
                  helperText="Empty uses the config file's gpg-bin."
                />
                <TextField
                  label="GPG home directory"
                  value={form.gpg_homedir}
                  onChange={(e) => set({ gpg_homedir: e.target.value })}
                  helperText="Empty uses the config file's gpg-homedir."
                />
              </Stack>
            </AccordionDetails>
          </Accordion>
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

export function JobConfigSection() {
  const [data, setData] = useState<JobConfigListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);

  // dialog is null while closed; editing is the job being edited, or null
  // when creating a new one.
  const [dialog, setDialog] = useState<{ editing: JobConfigJSON | null } | null>(null);
  const [form, setForm] = useState<JobForm>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<JobConfigJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<JobConfigListJSON>("/api/job-configs")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openDialog(editing: JobConfigJSON | null) {
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
        editing ? "/api/job-configs/" + encodeURIComponent(editing.name) : "/api/job-configs",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(definitionOf(form)),
        },
        "saving job failed",
      );
      setDialog(null);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  function remove(j: JobConfigJSON) {
    apiFetchOK(
      "/api/job-configs/" + encodeURIComponent(j.name),
      { method: "DELETE" },
      "deleting job failed",
    )
      .then(() => refresh())
      .catch((err: unknown) => setError(errorText(err)));
  }

  const jobs = data?.jobs ?? [];
  const editing = data?.editing ?? false;

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          A change applies from the job's next run on; a changed interval or start time reschedules
          it. A new job without a start time runs right away.
        </Typography>
        {editing ? (
          <Button
            variant="contained"
            startIcon={<AddIcon />}
            onClick={() => openDialog(null)}
            sx={{ flexShrink: 0 }}
          >
            New job
          </Button>
        ) : null}
      </Stack>

      {data && !editing ? <EditingDisabledNotice /> : null}
      {error ? <Alert severity="error">{error}</Alert> : null}

      {data && !jobs.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no jobs configured yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Name</TableCell>
                <TableCell>Command</TableCell>
                <TableCell>Schedule</TableCell>
                <TableCell>Targets</TableCell>
                <TableCell>Recipients</TableCell>
                {editing ? <TableCell>Updated</TableCell> : null}
                {editing ? <TableCell align="right" /> : null}
              </TableRow>
            </TableHead>
            <TableBody>
              {jobs.map((j) => (
                <TableRow key={j.name}>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {j.name}
                    {j.error ? (
                      <Tooltip title={j.error}>
                        <span>
                          <StatusChip state="failed" label="inactive" />
                        </span>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere", maxWidth: 280 }}>
                    <code>{j.command}</code>
                  </TableCell>
                  <TableCell>{scheduleText(j)}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {j.targets.map((t, i) => (
                      <Typography key={i} variant="caption" sx={{ display: "block" }}>
                        {t.server}/{t.bucket}
                        {t.retention ? ` (${t.retention})` : ""}
                      </Typography>
                    ))}
                  </TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>{j.recipients.join(", ")}</TableCell>
                  {editing ? (
                    <TableCell>
                      {fmtTime(j.updated_at)}
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ display: "block" }}
                      >
                        by {j.updated_by}
                      </Typography>
                    </TableCell>
                  ) : null}
                  {editing ? (
                    <TableCell align="right" sx={{ whiteSpace: "nowrap" }}>
                      <Tooltip title="Edit">
                        <IconButton size="small" onClick={() => openDialog(j)}>
                          <EditIcon fontSize="small" />
                        </IconButton>
                      </Tooltip>
                      <Tooltip title="Delete">
                        <IconButton size="small" color="error" onClick={() => setPendingDelete(j)}>
                          <DeleteIcon fontSize="small" />
                        </IconButton>
                      </Tooltip>
                    </TableCell>
                  ) : null}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}

      {dialog && data ? (
        <JobDialog
          editing={dialog.editing}
          form={form}
          data={data}
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
            Delete the job <strong>{pendingDelete?.name}</strong>? It stops being scheduled; a run
            in progress finishes. Its run history and the backups it wrote are kept.
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
