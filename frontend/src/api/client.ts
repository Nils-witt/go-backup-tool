import { getAccessToken, getIdToken, renewAccessToken } from "../auth/oidc";
import type { ClientConfigJSON, MeJSON, MetaJSON, SSOStatusJSON } from "./types";

/** Thrown on a non-2xx response; message is the server's own (plain-text) error body. */
export class ApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/**
 * Thrown when a change needs a more recent sign-in (an RFC 9470
 * insufficient_user_authentication challenge); maxAge is how recent, in
 * seconds. ReauthDialog offers to sign in again.
 */
export class ReauthRequiredError extends ApiError {
  maxAge: number;

  constructor(message: string, maxAge: number) {
    super(401, message);
    this.maxAge = maxAge;
  }
}

type ReauthListener = (err: ReauthRequiredError) => void;
const reauthListeners = new Set<ReauthListener>();

/** Registers cb to run whenever a change needs a more recent sign-in. Returns an unsubscribe function. */
export function onReauthRequired(cb: ReauthListener): () => void {
  reauthListeners.add(cb);
  return () => reauthListeners.delete(cb);
}

/** The max_age of an insufficient_user_authentication challenge, or null for any other 401. */
function reauthMaxAge(res: Response): number | null {
  const challenge = res.headers.get("WWW-Authenticate") ?? "";
  if (!challenge.includes('error="insufficient_user_authentication"')) return null;

  const m = /max_age=(\d+)/.exec(challenge);
  return m ? Number(m[1]) : 0;
}

async function send(url: string, opts: RequestInit, token: string | null): Promise<Response> {
  const headers = new Headers(opts.headers);
  if (token) headers.set("Authorization", "Bearer " + token);

  // Changes carry the ID token too: some of them need a recent sign-in,
  // which only its auth_time shows (see internal/backup/webui/reauth.go).
  if (token && opts.method && opts.method !== "GET") {
    const idToken = await getIdToken();
    if (idToken) headers.set("X-ID-Token", idToken);
  }

  return fetch(url, { ...opts, headers });
}

// apiFetch wraps fetch(), attaching the SSO access token (if any) as an
// Authorization header. A rejected token (expired, or revoked at the
// provider) gets one silent renewal attempt; a 401 that persists throws an
// ApiError rather than navigating anywhere — when the session can't be
// renewed, oidc.ts drops it and AuthContext/AuthGate send the browser to
// /login. A change that needs a more recent sign-in throws a
// ReauthRequiredError instead, without a renewal (which wouldn't help).
// Any other status is returned for the caller to inspect.
export async function apiFetch(url: string, opts: RequestInit = {}): Promise<Response> {
  let token = await getAccessToken();
  let res = await send(url, opts, token);

  const maxAge = res.status === 401 ? reauthMaxAge(res) : null;
  if (maxAge !== null) {
    const msg = (await res.text().catch(() => "")).trim();
    const err = new ReauthRequiredError(msg || "this change needs a more recent sign-in", maxAge);
    reauthListeners.forEach((cb) => cb(err));
    throw err;
  }

  if (res.status === 401 && token) {
    token = await renewAccessToken(true);
    if (token) res = await send(url, opts, token);
  }

  if (res.status === 401) {
    throw new ApiError(401, "unauthorized");
  }

  return res;
}

async function errorFrom(res: Response, fallback: string): Promise<ApiError> {
  const msg = (await res.text().catch(() => "")).trim();
  return new ApiError(res.status, msg || fallback);
}

export async function apiFetchJSON<T>(url: string, opts?: RequestInit): Promise<T> {
  const res = await apiFetch(url, opts);
  if (!res.ok) throw await errorFrom(res, `request failed: ${res.status}`);

  return (await res.json()) as T;
}

// apiFetchOK performs a mutating request and throws with the response body
// (or a fallback message) when it didn't succeed.
export async function apiFetchOK(
  url: string,
  opts: RequestInit,
  fallbackError: string,
): Promise<Response> {
  const res = await apiFetch(url, opts);
  if (!res.ok) throw await errorFrom(res, fallbackError);

  return res;
}

// publicJSON fetches one of the unauthenticated endpoints, which must work
// before any SSO session exists.
async function publicJSON<T>(url: string): Promise<T> {
  const res = await fetch(url);
  if (!res.ok) throw await errorFrom(res, `request failed: ${res.status}`);

  return (await res.json()) as T;
}

export const fetchMeta = () => publicJSON<MetaJSON>("/api/meta");
export const fetchSSOStatus = () => publicJSON<SSOStatusJSON>("/api/sso/status");
export const fetchClientConfig = () => publicJSON<ClientConfigJSON>("/api/client-config");
export const fetchMe = () => apiFetchJSON<MeJSON>("/api/me");
/** Records the just-completed SSO login in the login log; returns the account like fetchMe(). */
export const ssoLogin = () => apiFetchJSON<MeJSON>("/api/sso/login", { method: "POST" });
