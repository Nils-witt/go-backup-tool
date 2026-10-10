// SSO runs entirely in the browser: this SPA is a public OpenID Connect client
// (authorization code + PKCE, no client secret) talking to the provider
// directly. The backend never takes part in the login itself. It only
// verifies the provider-issued access token (a JWT), which client.ts sends as
// "Authorization: Bearer ..." on every /api/... call (see
// internal/backup/webui/auth.go).
//
// oidc-client-ts itself is only loaded (see initOidc) when SSO is enabled, so
// a password-only install never downloads it.
import type { UserManager } from "oidc-client-ts";
import type { SSOStatusJSON } from "../api/types";

export const SSO_CALLBACK_PATH = "/login/sso/callback";

let manager: UserManager | null = null;
let oidcLib: typeof import("oidc-client-ts") | null = null;
let onlineListenerAdded = false;

/**
 * Builds the UserManager from the public /api/sso/status settings. Called once
 * on app start (see AuthContext) before the first /api/me, so a token stored
 * in sessionStorage by an earlier page load is attached to it.
 */
export async function initOidc(status: SSOStatusJSON) {
  if (!status.enabled || !status.issuerUrl || !status.clientId) {
    manager = null;
    return;
  }

  oidcLib = await import("oidc-client-ts");

  manager = new oidcLib.UserManager({
    authority: status.issuerUrl,
    client_id: status.clientId,
    scope: status.scopes || "openid profile email",
    response_type: "code",
    redirect_uri: window.location.origin + SSO_CALLBACK_PATH,
    post_logout_redirect_uri: window.location.origin + "/login",
    // sessionStorage: the token survives a reload but not the browser session.
    userStore: new oidcLib.WebStorageStateStore({ store: window.sessionStorage }),
    // oidc-client-ts's built-in renewal falls back to loading the provider
    // in a hidden iframe when there's no refresh token, which providers that
    // send X-Frame-Options: deny (e.g. Authentik) refuse. Renewal is done by
    // hand instead, with refresh tokens only — see renewAccessToken.
    automaticSilentRenew: false,
  });

  // Refresh shortly before the token expires (60s by default), so requests
  // never have to wait for it. The expired event covers a refresh that
  // hasn't succeeded by then, and ends a session without a refresh token
  // even while the page sits idle. Both timers are armed by the first
  // getUser() and re-armed after every refresh.
  manager.events.addAccessTokenExpiring(() => {
    void renewAccessToken();
  });
  manager.events.addAccessTokenExpired(() => {
    void renewAccessToken();
  });

  // A refresh that failed while offline is retried as soon as the network
  // is back, rather than waiting out the backoff.
  if (!onlineListenerAdded) {
    onlineListenerAdded = true;
    window.addEventListener("online", () => {
      if (retryTimer !== undefined) void renewAccessToken();
    });
  }
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

const minRetryMs = 5_000;
const maxRetryMs = 60_000;
let retryTimer: ReturnType<typeof setTimeout> | undefined;
let retryDelay = minRetryMs;

/**
 * Gets a fresh access token with the stored refresh token (a plain back-channel
 * request to the provider's token endpoint — never an iframe). The provider
 * only issues a refresh token if asked for one, typically via the
 * "offline_access" scope. Concurrent callers share one in-flight refresh,
 * since a provider rotating refresh tokens would reject the second use of the
 * same one.
 *
 * Without a refresh token, the current token stays in use until it expires —
 * or until the backend rejects it (rejected=true, see client.ts). The session
 * then ends: the stored user is dropped (see onSsoSessionEnded) and null is
 * returned. The same happens when the provider refuses the refresh token.
 * A refresh that fails for any other reason (offline, provider unreachable)
 * keeps the session and is retried with backoff; meanwhile the current token
 * is returned while it's still valid, null otherwise.
 */
export function renewAccessToken(rejected = false): Promise<string | null> {
  renewing ??= (async () => {
    try {
      if (!manager) return null;

      const user = await manager.getUser();
      if (!user) return null;

      if (!user.refresh_token) {
        if (!rejected && !user.expired) return user.access_token;
        await endSession();
        return null;
      }

      try {
        const renewed = await manager.signinSilent();
        if (!renewed) throw new Error("no user returned");
        clearRetry();
        return renewed.access_token;
      } catch (err) {
        if (oidcLib !== null && err instanceof oidcLib.ErrorResponse) {
          await endSession();
          return null;
        }
        scheduleRetry();
        return !rejected && !user.expired ? user.access_token : null;
      }
    } finally {
      renewing = null;
    }
  })();

  return renewing;
}

function scheduleRetry() {
  clearTimeout(retryTimer);
  retryTimer = setTimeout(() => void renewAccessToken(), retryDelay);
  retryDelay = Math.min(retryDelay * 2, maxRetryMs);
}

function clearRetry() {
  clearTimeout(retryTimer);
  retryTimer = undefined;
  retryDelay = minRetryMs;
}

async function endSession() {
  clearRetry();
  await manager?.removeUser();
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
  clearRetry();
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
