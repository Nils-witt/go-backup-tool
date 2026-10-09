import { useState } from "react";
import { remoteErrorMessage, remoteFetchJSON } from "../api/client";
import type { JobSnapshot } from "../api/types";
import { normalizeBackendURL, useRemoteBackends } from "../lib/remoteBackends";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import DeleteIcon from "@mui/icons-material/Delete";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogContentText from "@mui/material/DialogContentText";
import DialogTitle from "@mui/material/DialogTitle";
import Divider from "@mui/material/Divider";
import IconButton from "@mui/material/IconButton";
import List from "@mui/material/List";
import ListItem from "@mui/material/ListItem";
import ListItemText from "@mui/material/ListItemText";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

// RemoteBackendsDialog manages the remote instances the dashboard merges
// into its own status (see lib/remoteBackends). Adding one first checks it
// is reachable with the given token, so a CORS or token problem shows here
// rather than as a silently missing instance.
export function RemoteBackendsDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [backends, setBackends] = useRemoteBackends();
  const [url, setURL] = useState("");
  const [token, setToken] = useState("");
  const [label, setLabel] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);

  async function add() {
    setError(null);

    const normalized = normalizeBackendURL(url);
    if (!normalized) {
      setError("Enter the remote's base URL, e.g. https://backup.example.com");
      return;
    }
    if (backends.some((b) => b.url === normalized)) {
      setError("That backend is already added.");
      return;
    }

    const backend = {
      id: crypto.randomUUID(),
      url: normalized,
      token: token.trim(),
      label: label.trim() || undefined,
    };

    setAdding(true);
    try {
      await remoteFetchJSON<JobSnapshot[]>(backend, "/api/status");
    } catch (err) {
      setError(remoteErrorMessage(err));
      return;
    } finally {
      setAdding(false);
    }

    setBackends([...backends, backend]);
    setURL("");
    setToken("");
    setLabel("");
  }

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>Remote backends</DialogTitle>
      <DialogContent>
        <DialogContentText variant="body2" sx={{ mb: 2 }}>
          Show other go-backup-tool instances' jobs and receivers on this dashboard. On each remote
          instance, create a token under <strong>API tokens</strong> and add{" "}
          <code>{window.location.origin}</code> to its <code>webui.cors-get-origins</code>. Tokens
          are stored only in this browser.
        </DialogContentText>

        {backends.length ? (
          <List dense disablePadding sx={{ mb: 2 }}>
            {backends.map((b) => (
              <ListItem
                key={b.id}
                disableGutters
                secondaryAction={
                  <IconButton
                    edge="end"
                    aria-label={"remove " + (b.label || b.url)}
                    onClick={() => setBackends(backends.filter((x) => x.id !== b.id))}
                  >
                    <DeleteIcon />
                  </IconButton>
                }
              >
                <ListItemText
                  primary={b.label || b.url}
                  secondary={b.label ? b.url : undefined}
                  slotProps={{ primary: { sx: { overflowWrap: "anywhere" } } }}
                />
              </ListItem>
            ))}
          </List>
        ) : (
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
            No remote backends added.
          </Typography>
        )}

        <Divider sx={{ mb: 2 }} />

        <Stack spacing={2}>
          {error ? <Alert severity="error">{error}</Alert> : null}
          <TextField
            label="URL"
            placeholder="https://backup.example.com"
            value={url}
            onChange={(e) => setURL(e.target.value)}
            required
          />
          <TextField
            label="API token"
            type="password"
            autoComplete="off"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            required
          />
          <TextField
            label="Label"
            helperText="Optional; defaults to the remote's instance name"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            slotProps={{ htmlInput: { maxLength: 100 } }}
          />
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Close</Button>
        <Button
          variant="contained"
          disabled={adding || !url.trim() || !token.trim()}
          onClick={() => void add()}
        >
          {adding ? "Checking…" : "Add"}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
