import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Typography from "@mui/material/Typography";
import LoginIcon from "@mui/icons-material/Login";
import { useAuth } from "../auth/useAuth";
import { startLogin } from "../auth/oidc";
import { AuthCard } from "../components/AuthCard";

export function LoginPage() {
  const [params] = useSearchParams();
  const next = params.get("next") || "/";
  const { ssoLabel } = useAuth();
  const [error, setError] = useState<string | null>(
    params.get("ssoerror") ? "SSO sign-in failed." : null,
  );

  async function handleSSOLogin() {
    try {
      await startLogin(next);
    } catch (err) {
      setError(`SSO sign-in failed: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  return (
    <AuthCard>
      <Typography variant="body2" color="text.secondary">
        Sign in to view the backup dashboard.
      </Typography>
      {error ? <Alert severity="error">{error}</Alert> : null}
      {ssoLabel ? (
        <Button variant="contained" startIcon={<LoginIcon />} onClick={() => void handleSSOLogin()}>
          {ssoLabel}
        </Button>
      ) : (
        <Alert severity="warning">
          Single sign-on is not configured, so signing in is not possible right now.
        </Alert>
      )}
    </AuthCard>
  );
}
