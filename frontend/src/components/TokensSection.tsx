import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { APITokenJSON, CreatedAPITokenJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogContentText from "@mui/material/DialogContentText";
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
import ContentCopyIcon from "@mui/icons-material/ContentCopy";

// LIFETIMES are the lifetime choices offered when issuing a token; the
// server accepts any whole number of days from 1 to 3650.
const LIFETIMES = [
  { days: 7, label: "7 days" },
  { days: 30, label: "30 days" },
  { days: 90, label: "90 days" },
  { days: 365, label: "1 year" },
  { days: 730, label: "2 years" },
  { days: 1825, label: "5 years" },
];

function tokenState(t: APITokenJSON): { state: string; label: string } {
  if (t.revoked_at) return { state: "failed", label: "revoked" };
  if (new Date(t.expires_at).getTime() <= Date.now()) return { state: "idle", label: "expired" };
  return { state: "ok", label: "active" };
}

export function TokensSection() {
  const [tokens, setTokens] = useState<APITokenJSON[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState("");
  const [lifetimeDays, setLifetimeDays] = useState(90);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const [created, setCreated] = useState<CreatedAPITokenJSON | null>(null);
  const [copied, setCopied] = useState(false);
  const [pendingRevoke, setPendingRevoke] = useState<APITokenJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<APITokenJSON[]>("/api/tokens")
      .then((d) => {
        setTokens(d || []);
        setLoaded(true);
        setError(null);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  function openCreate() {
    setName("");
    setLifetimeDays(90);
    setCreateError(null);
    setCreateOpen(true);
  }

  async function submitCreate() {
    setCreating(true);
    setCreateError(null);
    try {
      const res = await apiFetchOK(
        "/api/tokens",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: name.trim(), lifetime_days: lifetimeDays }),
        },
        "creating token failed",
      );
      setCreated((await res.json()) as CreatedAPITokenJSON);
      setCopied(false);
      setCreateOpen(false);
      refresh();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  }

  function revoke(t: APITokenJSON) {
    apiFetchOK("/api/tokens/" + encodeURIComponent(t.id), { method: "DELETE" }, "revoking failed")
      .then(() => refresh())
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)));
  }

  async function copyToken() {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.token);
      setCopied(true);
    } catch {
      /* clipboard unavailable (e.g. plain http): the field is still selectable. */
    }
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" sx={{ justifyContent: "space-between", alignItems: "center" }}>
        <Typography variant="body2" color="text.secondary">
          Tokens grant read-only access (view and job run history; no downloads or other audit
          logs). Send one as <code>Authorization: Bearer &lt;token&gt;</code>.
        </Typography>
        <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
          New token
        </Button>
      </Stack>

      {error ? <Alert severity="error">{error}</Alert> : null}

      {loaded && !tokens.length ? (
        <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
          no tokens issued yet
        </Typography>
      ) : (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Name</TableCell>
                <TableCell>Status</TableCell>
                <TableCell>Created</TableCell>
                <TableCell>Expires</TableCell>
                <TableCell align="right" />
              </TableRow>
            </TableHead>
            <TableBody>
              {tokens.map((t) => {
                const st = tokenState(t);
                return (
                  <TableRow key={t.id}>
                    <TableCell sx={{ overflowWrap: "anywhere" }}>{t.name}</TableCell>
                    <TableCell sx={{ whiteSpace: "nowrap" }}>
                      <StatusChip state={st.state} label={st.label} />
                      {t.revoked_at ? (
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          sx={{ display: "block" }}
                        >
                          {fmtTime(t.revoked_at)} by {t.revoked_by}
                        </Typography>
                      ) : null}
                    </TableCell>
                    <TableCell>
                      {fmtTime(t.created_at)}
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ display: "block" }}
                      >
                        by {t.created_by}
                      </Typography>
                    </TableCell>
                    <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(t.expires_at)}</TableCell>
                    <TableCell align="right">
                      {st.label === "active" ? (
                        <Button size="small" color="error" onClick={() => setPendingRevoke(t)}>
                          Revoke
                        </Button>
                      ) : null}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableContainer>
      )}

      <Dialog open={createOpen} onClose={() => setCreateOpen(false)} fullWidth maxWidth="xs">
        <DialogTitle>New API token</DialogTitle>
        <DialogContent>
          <Stack spacing={2} sx={{ pt: 1 }}>
            {createError ? <Alert severity="error">{createError}</Alert> : null}
            <TextField
              label="Name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              slotProps={{ htmlInput: { maxLength: 100 } }}
              autoFocus
              required
            />
            <TextField
              select
              label="Lifetime"
              value={lifetimeDays}
              onChange={(e) => setLifetimeDays(Number(e.target.value))}
            >
              {LIFETIMES.map((l) => (
                <MenuItem key={l.days} value={l.days}>
                  {l.label}
                </MenuItem>
              ))}
            </TextField>
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setCreateOpen(false)}>Cancel</Button>
          <Button
            variant="contained"
            disabled={creating || !name.trim()}
            onClick={() => void submitCreate()}
          >
            Create
          </Button>
        </DialogActions>
      </Dialog>

      <Dialog open={created !== null} fullWidth maxWidth="sm">
        <DialogTitle>Token created</DialogTitle>
        <DialogContent>
          <DialogContentText sx={{ mb: 2 }}>
            Copy the token for <strong>{created?.name}</strong> now. It can't be shown again.
          </DialogContentText>
          <TextField
            value={created?.token ?? ""}
            fullWidth
            multiline
            slotProps={{
              htmlInput: { readOnly: true, style: { fontFamily: "monospace", fontSize: ".8rem" } },
              input: {
                endAdornment: (
                  <InputAdornment position="end">
                    <Tooltip title={copied ? "Copied" : "Copy"}>
                      <IconButton onClick={() => void copyToken()} edge="end">
                        <ContentCopyIcon fontSize="small" />
                      </IconButton>
                    </Tooltip>
                  </InputAdornment>
                ),
              },
            }}
          />
        </DialogContent>
        <DialogActions>
          <Button variant="contained" onClick={() => setCreated(null)}>
            Done
          </Button>
        </DialogActions>
      </Dialog>

      <ConfirmDialog
        open={pendingRevoke !== null}
        message={
          <>
            Revoke the token <strong>{pendingRevoke?.name}</strong>? Anything using it loses access
            immediately.
          </>
        }
        confirmLabel="Revoke"
        onConfirm={() => {
          if (pendingRevoke) revoke(pendingRevoke);
          setPendingRevoke(null);
        }}
        onCancel={() => setPendingRevoke(null)}
      />
    </Stack>
  );
}
