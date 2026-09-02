import Button from "@mui/material/Button";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogContentText from "@mui/material/DialogContentText";
import TextField from "@mui/material/TextField";
import Alert from "@mui/material/Alert";
import { fmtTime } from "../lib/format";

export interface TokenResult {
  username: string;
  expiresAt: string;
  token: string;
  error: string;
}

// TokenResultDialog shows a freshly issued API token exactly once — the
// server never shows it again.
export function TokenResultDialog({
  result,
  onClose,
}: {
  result: TokenResult | null;
  onClose: () => void;
}) {
  function copy() {
    if (!result?.token || !navigator.clipboard?.writeText) return;
    navigator.clipboard.writeText(result.token).catch(() => {});
  }

  return (
    <Dialog open={result !== null} onClose={onClose} fullWidth maxWidth="sm">
      <DialogContent>
        <DialogContentText sx={{ mb: 2 }}>
          Token for <strong>{result?.username}</strong>
          {result?.expiresAt ? <>, valid until {fmtTime(result.expiresAt)}</> : null}. Copy it now —
          it won't be shown again.
        </DialogContentText>
        <TextField
          fullWidth
          multiline
          minRows={3}
          slotProps={{
            htmlInput: {
              readOnly: true,
              sx: { fontFamily: "ui-monospace, monospace", fontSize: ".8rem" },
            },
          }}
          value={result?.token ?? ""}
        />
        {result?.error ? (
          <Alert severity="error" sx={{ mt: 2 }}>
            {result.error}
          </Alert>
        ) : null}
      </DialogContent>
      <DialogActions>
        <Button onClick={copy}>Copy</Button>
        <Button onClick={onClose} variant="contained" autoFocus>
          Close
        </Button>
      </DialogActions>
    </Dialog>
  );
}
