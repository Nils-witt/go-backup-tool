import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { ApiError, fetchMe, fetchSSOStatus, ssoLogin } from "../api/client";
import type { MeJSON } from "../api/types";
import { initOidc, logoutOidc, onSsoSessionEnded } from "./oidc";

export interface AuthState {
  /** true once the initial /api/sso/status + /api/me round trip has settled. */
  ready: boolean;
  me: MeJSON | null;
  /** The SSO login button's label, or null when SSO is unavailable (nobody can sign in). */
  ssoLabel: string | null;
  /**
   * Reloads the current account; resolves to it, or null when not signed in.
   * Pass login=true only right after the provider callback, so the server
   * records the sign-in once (a plain reload is not logged).
   */
  refreshMe: (login?: boolean) => Promise<MeJSON | null>;
  logout: () => Promise<void>;

  // Display-time mirrors of the server's per-route permission checks — the
  // server enforces the same rules on every actual request, so these only
  // decide which sections/controls render.
  canDownload: boolean;
  // canRetry mirrors the server's admin gate on POST /api/jobs/{name}/retry.
  canRetry: boolean;
  canViewLoginLog: boolean;
  canViewDownloadLog: boolean;
  canViewJobRunLog: boolean;
  canViewTargetRunLog: boolean;
  canViewReceiverLog: boolean;
}

const AuthContext = createContext<AuthState | null>(null);

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
      canViewLoginLog: admin || has("login-log"),
      canViewDownloadLog: admin || has("download-log"),
      canViewJobRunLog: admin || has("job-run-log"),
      canViewTargetRunLog: admin || has("target-run-log"),
      canViewReceiverLog: admin || has("receiver-log"),
    };
  }, [ready, me, ssoLabel, refreshMe, logout]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
