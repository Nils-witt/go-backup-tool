import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { GPGKeyImportResultJSON, GPGKeyJSON, GPGKeyListJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
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
import AddIcon from "@mui/icons-material/Add";
import UploadFileIcon from "@mui/icons-material/UploadFile";

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// keyState summarizes whether a job can encrypt to k.
function keyState(k: GPGKeyJSON): { state: string; label: string } {
  if (k.revoked) return { state: "failed", label: "revoked" };
  if (k.expired) return { state: "failed", label: "expired" };
  if (k.disabled) return { state: "failed", label: "disabled" };
  if (!k.can_encrypt) return { state: "incomplete", label: "can't encrypt" };
  return { state: "ok", label: "usable" };
}

// groupFingerprint spaces a fingerprint into groups of four for reading.
function groupFingerprint(fpr: string): string {
  return fpr.replace(/(.{4})(?=.)/g, "$1 ");
}

function AddKeyDialog({
  onCancel,
  onImported,
}: {
  onCancel: () => void;
  onImported: (res: GPGKeyImportResultJSON) => void;
}) {
  const [armored, setArmored] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function readFile(file: File | undefined) {
    if (!file) return;
    setArmored(await file.text());
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      const res = await apiFetchJSON<GPGKeyImportResultJSON>("/api/gpg-keys", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ armored }),
      });
      onImported(res);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="md">
      <DialogTitle>Add GPG public key</DialogTitle>
      <DialogContent>
        <Stack spacing={2} sx={{ pt: 1 }}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <Typography variant="body2" color="text.secondary">
            Paste an ASCII-armored public key, e.g. the output of{" "}
            <code>gpg --export --armor you@example.com</code>, or load an <code>.asc</code> file.
            Private keys are refused.
          </Typography>
          <TextField
            label="Public key"
            value={armored}
            onChange={(e) => setArmored(e.target.value)}
            placeholder="-----BEGIN PGP PUBLIC KEY BLOCK-----"
            multiline
            minRows={8}
            maxRows={16}
            autoFocus
            slotProps={{
              htmlInput: { spellCheck: false, style: { fontFamily: "monospace", fontSize: 12 } },
            }}
          />
          <Button component="label" startIcon={<UploadFileIcon />} sx={{ alignSelf: "flex-start" }}>
            Load from file
            <input
              type="file"
              accept=".asc,.gpg,.pub,.txt,application/pgp-keys,text/plain"
              hidden
              onChange={(e) => void readFile(e.target.files?.[0])}
            />
          </Button>
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel}>Cancel</Button>
        <Button
          variant="contained"
          disabled={saving || !armored.trim()}
          onClick={() => void save()}
        >
          Add
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function GPGKeysSection() {
  const [data, setData] = useState<GPGKeyListJSON | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const [imported, setImported] = useState<GPGKeyImportResultJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<GPGKeyListJSON>("/api/gpg-keys")
      .then((d) => {
        setData(d);
        setError(null);
      })
      .catch((err: unknown) => setError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const keys = data?.keys ?? [];
  const editing = data?.editing ?? false;

  return (
    <Stack spacing={2}>
      <Stack
        direction={{ xs: "column", sm: "row" }}
        spacing={1}
        sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
      >
        <Typography variant="body2" color="text.secondary">
          Public keys in the keyring jobs encrypt backups to (
          {data?.homedir ? <code>{data.homedir}</code> : "gpg's default keyring"}). A job&apos;s
          recipients must each match one of them; jobs with their own GPG home directory use that
          keyring instead.
        </Typography>
        {editing ? (
          <Button
            variant="contained"
            startIcon={<AddIcon />}
            onClick={() => {
              setImported(null);
              setAdding(true);
            }}
            sx={{ flexShrink: 0 }}
          >
            Add key
          </Button>
        ) : null}
      </Stack>

      {data && !editing ? (
        <Alert severity="info">
          Read-only. A new key carrying a recipient&apos;s email address could receive that
          job&apos;s backups, so adding keys here needs <code>webui.job-editing: true</code> in the
          config file.
        </Alert>
      ) : null}
      {error ? <Alert severity="error">{error}</Alert> : null}
      {imported ? (
        <Alert
          severity={imported.warnings.length ? "warning" : "success"}
          onClose={() => setImported(null)}
        >
          Added {imported.fingerprints.length === 1 ? "key" : "keys"}{" "}
          {imported.fingerprints.map((f) => groupFingerprint(f)).join(", ")}.
          {imported.warnings.map((w) => (
            <Typography key={w} variant="body2" sx={{ mt: 1 }}>
              {w}
            </Typography>
          ))}
        </Alert>
      ) : null}

      {data && !keys.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no keys in the keyring yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>User IDs</TableCell>
                <TableCell>Fingerprint</TableCell>
                <TableCell>Algorithm</TableCell>
                <TableCell>Created</TableCell>
                <TableCell>Expires</TableCell>
                <TableCell>Status</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {keys.map((k) => {
                const s = keyState(k);
                return (
                  <TableRow key={k.fingerprint}>
                    <TableCell sx={{ overflowWrap: "anywhere" }}>
                      {k.user_ids.length
                        ? k.user_ids.map((u) => (
                            <Typography key={u} variant="body2">
                              {u}
                            </Typography>
                          ))
                        : "—"}
                    </TableCell>
                    <TableCell>
                      <code style={{ fontSize: 12 }}>{groupFingerprint(k.fingerprint)}</code>
                    </TableCell>
                    <TableCell sx={{ whiteSpace: "nowrap" }}>
                      {k.algorithm}
                      {k.length ? ` ${k.length}` : ""}
                    </TableCell>
                    <TableCell>{fmtTime(k.created)}</TableCell>
                    <TableCell>{k.expires ? fmtTime(k.expires) : "never"}</TableCell>
                    <TableCell>
                      <StatusChip state={s.state} label={s.label} />
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableContainer>
      )}

      {adding ? (
        <AddKeyDialog
          onCancel={() => setAdding(false)}
          onImported={(res) => {
            setAdding(false);
            setImported(res);
            refresh();
          }}
        />
      ) : null}
    </Stack>
  );
}
