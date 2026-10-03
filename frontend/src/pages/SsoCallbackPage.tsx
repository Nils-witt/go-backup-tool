import { useEffect, useRef } from "react";
import { useNavigate } from "react-router-dom";
import CircularProgress from "@mui/material/CircularProgress";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { useAuth } from "../auth/AuthContext";
import { completeLogin } from "../auth/oidc";
import { AuthCard } from "../components/AuthCard";

// SsoCallbackPage is where the OIDC provider redirects back to (see oidc.ts):
// oidc-client-ts exchanges the authorization code for tokens right here in
// the browser, and one POST /api/sso/login with the new access token both
// loads the account and records the login in the login log server-side
// (ordinary page loads use GET /api/me, which isn't logged).
export function SsoCallbackPage() {
  const { refreshMe } = useAuth();
  const navigate = useNavigate();
  // StrictMode runs effects twice in development; an authorization code can
  // only be redeemed once.
  const started = useRef(false);

  useEffect(() => {
    if (started.current) return;
    started.current = true;

    void (async () => {
      try {
        const next = await completeLogin();

        // The backend rejecting the fresh token (wrong client, SSO since
        // disabled, ...) is a failed login, not a silent bounce to /login.
        if (!(await refreshMe(true))) throw new Error("token rejected");
        navigate(next, { replace: true });
      } catch {
        navigate("/login?ssoerror=1", { replace: true });
      }
    })();
  }, [refreshMe, navigate]);

  return (
    <AuthCard>
      <Stack direction="row" spacing={2} sx={{ alignItems: "center" }}>
        <CircularProgress size={20} />
        <Typography variant="body2" color="text.secondary">
          Signing in…
        </Typography>
      </Stack>
    </AuthCard>
  );
}
