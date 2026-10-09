import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { ApiError, fetchMe, fetchSSOStatus, ssoLogin } from "../api/client";
import type { MeJSON } from "../api/types";
import { initOidc, logoutOidc, onSsoSessionEnded } from "./oidc";
import { AuthContext, type AuthState } from "./useAuth";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [ready, setReady] = useState(false);
  const [me, setMe] = useState<MeJSON | null>(null);
  const [ssoLabel, setSsoLabel] = useState<string | null>(null);

  const refreshMe = useCallback(async (login = false) => {
    try {
      const current = await (login ? ssoLogin() : fetchMe());
      setMe(current);
      return current;
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setMe(null);
        return null;
      }
      throw err;
    }
  }, []);

  useEffect(() => {
    void (async () => {
      // SSO settings first: a token stored by an earlier page load must be
      // attached to the very first /api/me (see oidc.ts / client.ts).
      try {
        const status = await fetchSSOStatus();
        initOidc(status);
        if (status.enabled) setSsoLabel(status.buttonLabel || "Sign in with SSO");
        // An SSO session that can't be renewed any more counts as logged
        // out: AuthGate then sends the user to /login?next=<current page>.
        onSsoSessionEnded(() => setMe(null));
      } catch {
        /* SSO status unavailable: the login page says so. */
      }

      try {
        await refreshMe();
      } catch {
        /* backend unreachable: treated as signed out. */
      }
      setReady(true);
    })();
  }, [refreshMe]);

  const logout = useCallback(async () => {
    setMe(null);
    // This may also navigate away, to the provider's logout.
    await logoutOidc();
  }, []);

  const value = useMemo<AuthState>(() => {
    const has = (p: string) => !!me?.permissions.includes(p);
    const admin = !!me?.admin;

    // Mirrors permission.Permission's Can* helpers: admin implies every
    // permission, and download implies view.
    return {
      ready,
      me,
      ssoLabel,
      refreshMe,
      logout,
      canDownload: admin || has("download"),
      canRetry: admin,
      canManageTokens: admin,
      canManageReceivers: admin,
      canManageSettings: admin,
      canViewLoginLog: admin || has("login-log"),
      canViewDownloadLog: admin || has("download-log"),
      canViewJobRunLog: admin || has("job-run-log"),
      canViewTargetRunLog: admin || has("target-run-log"),
      canViewReceiverLog: admin || has("receiver-log"),
    };
  }, [ready, me, ssoLabel, refreshMe, logout]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
