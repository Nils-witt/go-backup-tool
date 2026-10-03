// SSO runs entirely in the browser: this SPA is a public OpenID Connect client
// (authorization code + PKCE, no client secret) talking to the provider
// directly. The backend never takes part in the login itself. It only
// verifies the provider-issued access token (a JWT), which client.ts sends as
// "Authorization: Bearer ..." on every /api/... call (see
// internal/backup/webui/auth.go).
import { UserManager, WebStorageStateStore } from "oidc-client-ts";
import type { SSOStatusJSON } from "../api/types";

export const SSO_CALLBACK_PATH = "/login/sso/callback";

let manager: UserManager | null = null;

/**
 * Builds the UserManager from the public /api/sso/status settings. Called once
 * on app start (see AuthContext) before the first /api/me, so a token stored
 * in sessionStorage by an earlier page load is attached to it.
 */
export function initOidc(status: SSOStatusJSON) {
  if (!status.enabled || !status.issuerUrl || !status.clientId) {
    manager = null;
    return;
  }

  manager = new UserManager({
    authority: status.issuerUrl,
    client_id: status.clientId,
    scope: status.scopes || "openid profile email",
    response_type: "code",
    redirect_uri: window.location.origin + SSO_CALLBACK_PATH,
    post_logout_redirect_uri: window.location.origin + "/login",
    // sessionStorage: the token survives a reload but not the browser session.
    userStore: new WebStorageStateStore({ store: window.sessionStorage }),
    // oidc-client-ts's built-in renewal falls back to loading the provider
    // in a hidden iframe when there's no refresh token, which providers that
    // send X-Frame-Options: deny (e.g. Authentik) refuse. Renewal is done by
    // hand instead, with refresh tokens only — see renewAccessToken.
    automaticSilentRenew: false,
  });

  manager.events.addAccessTokenExpiring(() => {
    void renewAccessToken();
  });
}

/**
 * Registers cb to run when the SSO session ends on its own (renewal
 * impossible or failed, so the stored token was dropped). Returns an
 * unsubscribe function.
 */
export function onSsoSessionEnded(cb: () => void): () => void {
  if (!manager) return () => {};

  return manager.events.addUserUnloaded(cb);
}

/**
 * The current access token, renewed first if it has already expired, or null
 * if not signed in via SSO (or the session has ended).
 */
export async function getAccessToken(): Promise<string | null> {
  if (!manager) return null;

  const user = await manager.getUser();
  if (!user) return null;
  if (user.expired) return renewAccessToken();

  return user.access_token;
}

let renewing: Promise<string | null> | null = null;

/**
 * Gets a fresh access token with the stored refresh token (a plain back-channel
 * request to the provider's token endpoint — never an iframe). The provider
 * only issues a refresh token if asked for one, typically via the
 * "offline_access" scope. Without one, or if the refresh fails, the stored
 * user is dropped (ending the session, see onSsoSessionEnded) and null is
 * returned. Concurrent callers share one in-flight refresh, since a provider
 * rotating refresh tokens would reject the second use of the same one.
 */
export function renewAccessToken(): Promise<string | null> {
  renewing ??= (async () => {
    try {
      if (!manager) return null;

      const user = await manager.getUser();
      if (!user) return null;

      if (user.refresh_token) {
        try {
          const renewed = await manager.signinSilent();
          if (renewed) return renewed.access_token;
        } catch {
          /* fall through: session over */
        }
      }

      await manager.removeUser();
      return null;
    } finally {
      renewing = null;
    }
  })();

  return renewing;
}

/** Sends the browser to the provider's login page. */
export async function startLogin(next: string) {
  if (!manager) throw new Error("SSO is not enabled");

  await manager.signinRedirect({ state: { next } });
}

/**
 * Handles the provider's redirect back to SSO_CALLBACK_PATH and returns the
 * same-site path to continue to.
 */
export async function completeLogin(): Promise<string> {
  if (!manager) throw new Error("SSO is not enabled");

  const user = await manager.signinRedirectCallback();
  const next = (user.state as { next?: unknown } | undefined)?.next;
  return safeNext(typeof next === "string" ? next : "/");
}

/**
 * Forgets the SSO token locally, then ends the provider session too if the
 * provider supports it (this navigates away from the app). Returns whether a
 * redirect was started.
 */
export async function logoutOidc(): Promise<boolean> {
  if (!manager) return false;

  const user = await manager.getUser();
  await manager.removeUser();
  if (!user) return false;

  try {
    const endSession = await manager.metadataService.getEndSessionEndpoint();
    if (!endSession) return false;

    await manager.signoutRedirect({ id_token_hint: user.id_token });
    return true;
  } catch {
    return false;
  }
}

/** A same-site relative path, or '/'. "//host" is protocol-relative, so it would leave the site. */
export function safeNext(next: string): string {
  return next.startsWith("/") && !next.startsWith("//") ? next : "/";
}
