import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type {
  JobConfigJSON,
  JobConfigListJSON,
  JobDefinitionJSON,
  JobFileJSON,
  JobKind,
  JobSourceJSON,
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
import ArrowUpwardIcon from "@mui/icons-material/ArrowUpward";
import DeleteIcon from "@mui/icons-material/Delete";
import EditIcon from "@mui/icons-material/Edit";
import ExpandMoreIcon from "@mui/icons-material/ExpandMore";
import Box from "@mui/material/Box";

// JobForm is the create/edit dialog's state: a job's definition, with its
// lists as editable text (one entry per line).
interface JobForm extends Omit<JobDefinitionJSON, "recipients" | "type" | "source"> {
  type: JobKind;
  recipients: string;
  source: SourceForm;
}

// SourceForm is an on-change job's source as edited: excludes one per line.
interface SourceForm extends Omit<JobSourceJSON, "exclude"> {
  exclude: string;
}

const EMPTY_SOURCE: SourceForm = { path: "", exclude: "", watch: true, debounce: "" };

const EMPTY_TARGET: JobTargetJSON = {
  server: "",
  bucket: "",
  retention: "",
  on_error: null,
  on_recover: null,
};

const EMPTY_FILE: JobFileJSON = { command: "", name: "" };

const EMPTY_FORM: JobForm = {
  name: "",
  type: "backup",
  source: EMPTY_SOURCE,
  files: [EMPTY_FILE],
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
    type: j.type || "backup",
    source: j.source
      ? {
          path: j.source.path,
          exclude: (j.source.exclude ?? []).join("\n"),
          watch: j.source.watch,
          debounce: j.source.debounce ?? "",
        }
      : EMPTY_SOURCE,
    files: j.files?.length
      ? j.files.map((f) => ({ command: f.command, name: f.name ?? "" }))
      : [EMPTY_FILE],
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

function splitLines(text: string, separators = /\n/): string[] {
  return text
    .split(separators)
    .map((r) => r.trim())
    .filter(Boolean);
}

function definitionOf(form: JobForm): JobDefinitionJSON {
  const recipients = splitLines(form.recipients, /[\n,]/);
  if (form.type === "on-change") {
    // An on-change job uploads its source; files and key don't apply.
    return {
      ...form,
      name: form.name.trim(),
      files: [],
      key: "",
      recipients,
      source: {
        path: form.source.path.trim(),
        exclude: splitLines(form.source.exclude),
        watch: form.source.watch,
        debounce: form.source.debounce.trim(),
      },
    };
  }
  return {
    ...form,
    name: form.name.trim(),
    files: form.files.map((f) => {
      const name = f.name?.trim();
      return name ? { command: f.command, name } : { command: f.command };
    }),
    recipients,
    source: null,
  };
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function scheduleText(j: JobDefinitionJSON): string {
  if (j.type === "on-change") {
    const watch = j.source?.watch ? "on change" : "";
    const scan = j.interval ? `scan every ${j.interval}` : "";
    return [watch, scan].filter(Boolean).join(", ") || "once";
  }
  if (!j.interval) return "once";
  return j.start_time ? `every ${j.interval} from ${fmtTime(j.start_time)}` : `every ${j.interval}`;
}

// fileName is the name f is stored as under the backup's key: its own, or
// the server's default (see config.resolveJobSources).
function fileName(f: JobFileJSON, armor: boolean): string {
  return f.name?.trim() || `${f.command}${armor ? ".asc" : ".gpg"}`;
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
      sx={{ minWidth: 160, flex: 1 }}
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

// FilesEditor edits a job's files: one row per command, in run order, each
// with an optional file name.
function FilesEditor({
  files,
  commands,
  armor,
  onChange,
}: {
  files: JobFileJSON[];
  commands: string[];
  armor: boolean;
  onChange: (files: JobFileJSON[]) => void;
}) {
  const setFile = (i: number, patch: Partial<JobFileJSON>) =>
    onChange(files.map((old, j) => (j === i ? { ...old, ...patch } : old)));
  const moveUp = (i: number) =>
    onChange(files.map((f, j) => (j === i - 1 ? files[i] : j === i ? files[i - 1] : f)));
  const names = files.map((f) => fileName(f, armor));

  return (
    <Stack spacing={1}>
      <Typography variant="subtitle2">Files</Typography>
      <Typography variant="caption" color="text.secondary">
        Each command&apos;s standard output is encrypted and stored as one file under the
        backup&apos;s key. Commands run one after another, in this order. Manage commands on the
        Commands page.
      </Typography>
      {files.map((f, i) => {
        const duplicate = !!f.command && names.indexOf(names[i]) !== i;
        return (
          <Box
            key={i}
            sx={{
              alignItems: { sm: "flex-start" },
              display: "flex",
              flexDirection: { xs: "column", sm: "row" },
              gap: 1,
              width: "100%",
            }}
          >
            <CommandSelect
              label="Command"
              commands={commands}
              value={f.command}
              onChange={(command) => setFile(i, { command })}
              required
            />
            <TextField
              size="small"
              label="File name"
              value={f.name ?? ""}
              onChange={(e) => setFile(i, { name: e.target.value })}
              placeholder={f.command ? fileName({ command: f.command }, armor) : "<command>.gpg"}
              error={duplicate}
              helperText={duplicate ? "Another file already has this name" : undefined}
              slotProps={{ htmlInput: { spellCheck: false } }}
              sx={{ flex: 1 }}
            />
            <Stack direction="row" sx={{ alignSelf: { xs: "flex-end", sm: "center" } }}>
              <Tooltip title="Move up">
                <span>
                  <IconButton size="small" onClick={() => moveUp(i)} disabled={i === 0}>
                    <ArrowUpwardIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
              <Tooltip title="Remove file">
                <span>
                  <IconButton
                    size="small"
                    onClick={() => onChange(files.filter((_, j) => j !== i))}
                    disabled={files.length === 1}
                  >
                    <DeleteIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            </Stack>
          </Box>
        );
      })}
      <Button
        startIcon={<AddIcon />}
        onClick={() => onChange([...files, EMPTY_FILE])}
        sx={{ alignSelf: "flex-start" }}
      >
        Add file
      </Button>
    </Stack>
  );
}

// SourceEditor edits an on-change job's source: the directory whose files
// are uploaded one by one, and how changes are picked up.
function SourceEditor({
  source,
  onChange,
}: {
  source: SourceForm;
  onChange: (s: SourceForm) => void;
}) {
  const set = (patch: Partial<SourceForm>) => onChange({ ...source, ...patch });

  return (
    <Stack spacing={2}>
      <Typography variant="subtitle2">Source</Typography>
      <Typography variant="caption" color="text.secondary">
        Every file under this directory is encrypted on its own and uploaded to directory receivers.
        A file is only uploaded again once its content changes; the receiver keeps each content as a
        new version.
      </Typography>
      <TextField
        label="Directory"
        value={source.path}
        onChange={(e) => set({ path: e.target.value })}
        placeholder="/srv/documents"
        helperText="Absolute path on this machine."
        required
        slotProps={{ htmlInput: { spellCheck: false } }}
      />
      <TextField
        label="Exclude"
        value={source.exclude}
        onChange={(e) => set({ exclude: e.target.value })}
        helperText='Patterns matched against the relative path or the name, one per line, e.g. "*.tmp" or "cache". A matching directory is skipped entirely.'
        multiline
        minRows={2}
        slotProps={{ htmlInput: { spellCheck: false } }}
      />
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={2}
        sx={{ alignItems: { sm: "center" } }}
      >
        <FormControlLabel
          control={
            <Switch checked={source.watch} onChange={(e) => set({ watch: e.target.checked })} />
          }
          label="Upload changes as they happen"
        />
        <TextField
          size="small"
          label="Debounce"
          value={source.debounce}
          onChange={(e) => set({ debounce: e.target.value })}
          placeholder="5s"
          helperText="Wait this long after the last change."
          disabled={!source.watch}
          sx={{ width: { sm: 200 } }}
        />
      </Stack>
    </Stack>
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
  const isOnChange = form.type === "on-change";
  const fileNames = form.files.map((f) => fileName(f, form.armor));
  // An on-change job uploads only to directory receivers, i.e. remote servers.
  const servers = isOnChange ? data.servers.filter((s) => s.type === "remote") : data.servers;
  const sourceOK = !!form.source.path.trim() && (form.source.watch || !!form.interval.trim());
  const filesOK =
    form.files.length > 0 &&
    form.files.every((f) => f.command) &&
    new Set(fileNames).size === fileNames.length;
  const canSave =
    !!form.name.trim() &&
    (isOnChange ? sourceOK : filesOK) &&
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
          <TextField
            select
            label="Type"
            value={form.type}
            onChange={(e) => set({ type: e.target.value as JobKind })}
            helperText={
              isOnChange
                ? "Uploads each changed file of a directory, encrypted on its own, to directory receivers."
                : "Runs its commands and uploads their encrypted output as one backup."
            }
          >
            <MenuItem value="backup">Backup</MenuItem>
            <MenuItem value="on-change">On change</MenuItem>
          </TextField>
          {isOnChange ? (
            <SourceEditor source={form.source} onChange={(source) => set({ source })} />
          ) : (
            <>
              {!data.commands.length ? (
                <Alert severity="warning">No commands yet — create one on the Commands page.</Alert>
              ) : null}
              <FilesEditor
                files={form.files}
                commands={data.commands}
                armor={form.armor}
                onChange={(files) => set({ files })}
              />
              <TextField
                label="Key"
                value={form.key}
                onChange={(e) => set({ key: e.target.value })}
                placeholder="backup-{time}"
                helperText="The backup's name: a folder holding one file per command. {time} becomes the run's UTC timestamp, so repeats don't overwrite each other."
                slotProps={{ htmlInput: { spellCheck: false } }}
              />
            </>
          )}
          <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
            <TextField
              label={isOnChange ? "Full scan interval" : "Interval"}
              value={form.interval}
              onChange={(e) => set({ interval: e.target.value })}
              helperText={
                isOnChange
                  ? 'Also scan everything every, e.g. "6h". Empty scans only at start.'
                  : 'Repeat every, e.g. "6h". Empty runs once.'
              }
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
            {!servers.length ? (
              <Alert severity="warning">
                {isOnChange
                  ? "No remote servers yet — an on-change job uploads to a directory receiver on a remote server. Create one on the Servers page."
                  : "No servers yet — create one on the Servers page."}
              </Alert>
            ) : null}
            {form.targets.map((t, i) => (
              <TargetEditor
                key={i}
                target={t}
                servers={servers}
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
            helperText="GPG keys to encrypt to, one per line, preferably by fingerprint. Each must match exactly one usable key in the keyring, or the job fails."
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
                <TableCell>Files</TableCell>
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
                    {j.type === "on-change" && j.source ? (
                      <Typography variant="caption" sx={{ display: "block" }}>
                        on change: <code>{j.source.path}</code>
                      </Typography>
                    ) : null}
                    {(j.files ?? []).map((f, i) => (
                      <Typography key={i} variant="caption" sx={{ display: "block" }}>
                        <code>{f.command}</code> → {fileName(f, j.armor)}
                      </Typography>
                    ))}
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
