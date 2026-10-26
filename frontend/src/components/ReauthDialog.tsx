import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import Alert from "@mui/material/Alert";
import { onReauthRequired } from "../api/client";
import { startReauth } from "../auth/oidc";
import { ConfirmDialog } from "./ConfirmDialog";

// ReauthDialog offers to sign in again when a change needed a more recent
// sign-in than the current session's (changes to jobs, servers, commands,
// and GPG keys; see internal/backup/webui/reauth.go). The form stays open
// behind it, so cancelling keeps what was entered.
export function ReauthDialog() {
  const location = useLocation();
  const [maxAge, setMaxAge] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => onReauthRequired((err) => setMaxAge(err.maxAge)), []);

  const minutes = maxAge ? Math.max(1, Math.round(maxAge / 60)) : null;

  return (
    <ConfirmDialog
      open={maxAge !== null}
      message={
        <>
          This change needs a sign-in
          {minutes ? ` within the last ${minutes} minute${minutes === 1 ? "" : "s"}` : ""}. Sign in
          again, then repeat it: unsaved changes on this page are lost.
        </>
      }
      confirmLabel="Sign in again"
      onConfirm={() => {
        startReauth(location.pathname + location.search, maxAge ?? 0).catch((err: unknown) =>
          setError(err instanceof Error ? err.message : String(err)),
        );
      }}
      onCancel={() => {
        setMaxAge(null);
        setError(null);
      }}
    >
      {error ? <Alert severity="error">{error}</Alert> : null}
    </ConfirmDialog>
  );
}
