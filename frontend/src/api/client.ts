import { getAccessToken, renewAccessToken } from "../auth/oidc";
import type { MeJSON, MetaJSON, SSOStatusJSON } from "./types";

/** Thrown on a non-2xx response; message is the server's own (plain-text) error body. */
export class ApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function send(url: string, opts: RequestInit, token: string | null): Promise<Response> {
  const headers = new Headers(opts.headers);
  if (token) headers.set("Authorization", "Bearer " + token);

  return fetch(url, { ...opts, headers });
}

// apiFetch wraps fetch(), attaching the SSO access token (if any) as an
// Authorization header. A rejected token (expired, or revoked at the
// provider) gets one silent renewal attempt; a 401 that persists throws an
// ApiError rather than navigating anywhere — when the session can't be
// renewed, oidc.ts drops it and AuthContext/AuthGate send the browser to
// /login. Any other status is returned for the caller to inspect.
export async function apiFetch(url: string, opts: RequestInit = {}): Promise<Response> {
  let token = await getAccessToken();
  let res = await send(url, opts, token);

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

// remoteFetchJSON GETs path from another instance (see lib/remoteBackends)
// with that instance's API token — never this instance's SSO token. A
// network failure, which is also how a browser reports a CORS refusal,
// rejects with a TypeError rather than an ApiError.
export async function remoteFetchJSON<T>(
  backend: { url: string; token: string },
  path: string,
): Promise<T> {
  const res = await fetch(backend.url + path, {
    headers: { Authorization: "Bearer " + backend.token },
  });
  if (!res.ok) throw await errorFrom(res, `request failed: ${res.status}`);

  return (await res.json()) as T;
}

// remoteErrorMessage turns a remoteFetchJSON failure into something an
// operator can act on.
export function remoteErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 401) return "API token rejected (invalid, revoked or expired)";
    return `${err.status}: ${err.message}`;
  }
  if (err instanceof TypeError) {
    return `unreachable, or this origin (${window.location.origin}) is not in the remote's webui.cors-get-origins`;
  }

  return String(err);
}

export const fetchRemoteMeta = (backend: { url: string }) =>
  publicJSON<MetaJSON>(backend.url + "/api/meta");

export const fetchMeta = () => publicJSON<MetaJSON>("/api/meta");
export const fetchSSOStatus = () => publicJSON<SSOStatusJSON>("/api/sso/status");
export const fetchMe = () => apiFetchJSON<MeJSON>("/api/me");
/** Records the just-completed SSO login in the login log; returns the account like fetchMe(). */
export const ssoLogin = () => apiFetchJSON<MeJSON>("/api/sso/login", { method: "POST" });
