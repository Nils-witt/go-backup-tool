import { useCallback, useEffect, useMemo, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type {
  CommandConfigJSON,
  CommandConfigListJSON,
  CommandDefinitionJSON,
  DockerContainerJSON,
  DockerContainerListJSON,
} from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { EditingDisabledNotice } from "./EditingDisabledNotice";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Autocomplete from "@mui/material/Autocomplete";
import Button from "@mui/material/Button";
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

const EMPTY_FORM: CommandDefinitionJSON = {
  id: "",
  type: "shell",
  cmd: "",
  timeout: "",
  container: "",
  container_user: "",
  path: "",
  exclude: [],
  compress: "",
};

// toPayload drops the fields form's type doesn't use (the backend rejects a
// command that mixes them) and blank exclude lines.
function toPayload(form: CommandDefinitionJSON): CommandDefinitionJSON {
  const base = { ...EMPTY_FORM, id: form.id.trim(), type: form.type, timeout: form.timeout };
  if (form.type === "tar") {
    return {
      ...base,
      path: form.path,
      exclude: (form.exclude ?? []).map((p) => p.trim()).filter(Boolean),
      compress: form.compress,
    };
  }
  return { ...base, cmd: form.cmd, container: form.container, container_user: form.container_user };
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// useDockerContainers loads the running containers on this instance's Docker
// daemon for the container picker. error is set when the daemon can't be
// reached (e.g. Docker isn't installed), which leaves the field free-text.
function useDockerContainers() {
  const [containers, setContainers] = useState<DockerContainerJSON[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    apiFetchJSON<DockerContainerListJSON>("/api/docker-containers")
      .then((d) => {
        if (!cancelled) setContainers(d.containers);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setContainers([]);
        setError(errorText(err));
      });

    return () => {
      cancelled = true;
    };
  }, []);

  return { containers, error };
}

// ContainerFields edits a shell command's container and container user,
// offering the Docker daemon's running containers.
function ContainerFields({
  form,
  set,
}: {
  form: CommandDefinitionJSON;
  set: (patch: Partial<CommandDefinitionJSON>) => void;
}) {
  const docker = useDockerContainers();
  const containersByName = useMemo(
    () => new Map((docker.containers ?? []).map((c) => [c.name, c])),
    [docker.containers],
  );
  const containerHelp =
    "Run inside this running container (name or ID) through the Docker socket. Empty runs it here.";

  return (
    <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
      <Autocomplete
        freeSolo
        options={[...containersByName.keys()]}
        loading={docker.containers === null}
        value={form.container || null}
        inputValue={form.container}
        onChange={(_, v) => set({ container: v ?? "" })}
        onInputChange={(_, v) => set({ container: v })}
        renderOption={({ key, ...props }, name) => {
          const c = containersByName.get(name);
          return (
            <li key={key} {...props}>
              <span>
                {name}
                {c ? (
                  <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
                    {c.image} · {c.status}
                  </Typography>
                ) : null}
              </span>
            </li>
          );
        }}
        renderInput={(params) => (
          <TextField
            {...params}
            label="Container"
            helperText={
              docker.error
                ? `${containerHelp} Couldn't list containers: ${docker.error}`
                : containerHelp
            }
            slotProps={{
              ...params.slotProps,
              htmlInput: { ...params.slotProps.htmlInput, spellCheck: false },
            }}
          />
        )}
        sx={{ flex: 2 }}
      />
      <TextField
        label="Container user"
        value={form.container_user}
        onChange={(e) => set({ container_user: e.target.value })}
        helperText="user[:group]. Empty uses the container's default."
        disabled={!form.container.trim()}
        slotProps={{ htmlInput: { spellCheck: false } }}
        sx={{ flex: 1 }}
      />
    </Stack>
  );
}

// TarFields edits a tar command's path, exclude globs and compression.
function TarFields({
  form,
  set,
}: {
  form: CommandDefinitionJSON;
  set: (patch: Partial<CommandDefinitionJSON>) => void;
}) {
  return (
    <>
      <TextField
        label="Path"
        value={form.path}
        onChange={(e) => set({ path: e.target.value })}
        helperText="Absolute path of the file or directory on this machine to archive. Symlinks inside it are stored as links."
        required
        slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: "monospace" } } }}
      />
      <TextField
        label="Exclude"
        value={(form.exclude ?? []).join("\n")}
        onChange={(e) => set({ exclude: e.target.value.split("\n") })}
        helperText={`One glob per line, matched against each entry's path relative to Path and against its name, e.g. "*.log" or "cache". A matching directory is left out entirely.`}
        multiline
        minRows={2}
        slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: "monospace" } } }}
      />
      <FormControlLabel
        control={
          <Switch
            checked={form.compress === "gzip"}
            onChange={(e) => set({ compress: e.target.checked ? "gzip" : "" })}
          />
        }
        label="Compress with gzip"
      />
    </>
  );
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
  const tar = form.type === "tar";
  const canSave = !!form.id.trim() && (tar ? !!form.path.trim() : !!form.cmd.trim());

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
            helperText="What a job (as its backup source) or a job target's on-error/on-recover names. Can't be changed later."
            disabled={!!editing}
            autoFocus={!editing}
            required
          />
          <TextField
            select
            label="Type"
            value={tar ? "tar" : "shell"}
            onChange={(e) => set({ type: e.target.value === "tar" ? "tar" : "shell" })}
            helperText={
              tar
                ? "Archives a path on this machine; the tar archive is the backup. Only usable as a job's backup source, not as an on-error/on-recover hook."
                : "Runs a shell command."
            }
          >
            <MenuItem value="shell">Shell command</MenuItem>
            <MenuItem value="tar">Tar archive of a path</MenuItem>
          </TextField>
          {tar ? (
            <TarFields form={form} set={set} />
          ) : (
            <>
              <TextField
                label="Command"
                value={form.cmd}
                onChange={(e) => set({ cmd: e.target.value })}
                helperText="Run through the shell. As a job's source, its standard output is the backup. As an on-error/on-recover hook, context comes in GBT_EVENT, GBT_JOB, GBT_TARGET, GBT_SERVER, GBT_ERROR, GBT_CONSECUTIVE_FAILURES, and GBT_TIME."
                required
                multiline
                minRows={2}
                slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: "monospace" } } }}
              />
              <ContainerFields form={form} set={set} />
            </>
          )}
          <TextField
            label="Timeout"
            value={form.timeout}
            onChange={(e) => set({ timeout: e.target.value })}
            helperText={
              tar
                ? 'How long one archive may take, e.g. "15m" or "2h". Empty: no limit.'
                : 'How long one run may take, e.g. "15s" or "2h". Empty: no limit as a job source, 30s as a hook.'
            }
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

// CommandSummary is a command's cell in the commands table: what it runs,
// or what it archives.
function CommandSummary({ c }: { c: CommandConfigJSON }) {
  if (c.type === "tar") {
    const exclude = c.exclude ?? [];
    const details = [
      exclude.length ? `excluding ${exclude.join(", ")}` : "",
      c.compress === "gzip" ? "gzip" : "",
    ].filter(Boolean);
    return (
      <>
        <code>tar {c.path}</code>
        {details.length ? (
          <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
            {details.join(" · ")}
          </Typography>
        ) : null}
      </>
    );
  }

  return (
    <>
      <code>{c.cmd}</code>
      {c.container ? (
        <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
          in container {c.container}
          {c.container_user ? ` as ${c.container_user}` : ""}
        </Typography>
      ) : null}
    </>
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
    setForm(
      editing
        ? {
            id: editing.id,
            type: editing.type === "tar" ? "tar" : "shell",
            cmd: editing.cmd,
            timeout: editing.timeout,
            container: editing.container ?? "",
            container_user: editing.container_user ?? "",
            path: editing.path ?? "",
            exclude: editing.exclude ?? [],
            compress: editing.compress ?? "",
          }
        : EMPTY_FORM,
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
        editing ? "/api/command-configs/" + encodeURIComponent(editing.id) : "/api/command-configs",
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(toPayload(form)),
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
          A job's backup sources (a shell command's output, or a tar archive of a path), and the
          shell commands a job's targets run after repeated failures (on-error) or once they recover
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
                    <CommandSummary c={c} />
                  </TableCell>
                  <TableCell>{c.timeout || (c.type === "tar" ? "—" : "30s")}</TableCell>
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
