import { createContext, useContext } from "react";
import type { MeJSON } from "../api/types";

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

// AuthContext carries the AuthState AuthProvider (AuthContext.tsx) builds.
// Lives here, apart from the provider component, so that module exports
// only components (React fast refresh needs component-only modules).
export const AuthContext = createContext<AuthState | null>(null);

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
