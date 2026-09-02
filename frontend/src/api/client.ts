// Direct port of dashboard.js's token/fetch helpers (lines 1-56) — the
// sessionStorage key and the 401-redirects-to-login contract must stay
// byte-for-byte identical to what login.html/oidc_complete.html (both
// untouched, server-rendered pages outside this SPA) write and expect.

export const TOKEN_KEY = "gbt_webui_token";

export function getToken(): string {
  try {
    return sessionStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}

export function setToken(token: string): void {
  try {
    sessionStorage.setItem(TOKEN_KEY, token);
  } catch {
    // sessionStorage unavailable (e.g. private browsing) — nothing to do.
  }
}

export function clearToken(): void {
  try {
    sessionStorage.removeItem(TOKEN_KEY);
  } catch {
    // ignore
  }
}

// goToLogin clears the stored token and sends the browser to the login
// page, remembering the current path so a successful login returns here.
export function goToLogin(): void {
  clearToken();
  if (import.meta.env.DEV) {
    console.log("redirecting to /login?next=" + window.location.pathname);
    return;
  }
  window.location.href = "/login?next=" + encodeURIComponent(window.location.pathname);
}

// apiFetch wraps fetch(), attaching the stored bearer token (if any) as an
// Authorization header. A 401 means the token is missing/invalid/expired —
// there's no server-side redirect to fall back on — so this sends the
// browser to /login itself instead of letting the caller deal with it.
export async function apiFetch(url: string, opts: RequestInit = {}): Promise<Response> {
  const headers = new Headers(opts.headers);
  const token = getToken();
  if (token) headers.set("Authorization", "Bearer " + token);
  if (import.meta.env.DEV) {
    url = url.startsWith("/") ? "http://localhost:8082" + url : url;
  }

  const res = await fetch(url, { ...opts, headers });
  if (res.status === 401) {
    goToLogin();
    throw new Error("unauthorized");
  }

  return res;
}

export async function apiFetchJSON<T>(url: string, opts?: RequestInit): Promise<T> {
  const res = await apiFetch(url, opts);
  return (await res.json()) as T;
}

// apiFetchOK performs a mutating request and throws with the response body
// (or a fallback message) when it didn't succeed — matching the
// r.ok/r.text()-then-throw pattern dashboard.js uses for form submissions
// (add user, issue token, set OIDC permissions).
export async function apiFetchOK(
  url: string,
  opts: RequestInit,
  fallbackError: string,
): Promise<Response> {
  const res = await apiFetch(url, opts);
  if (!res.ok) {
    const msg = await res.text().catch(() => "");
    throw new Error(msg || fallbackError);
  }

  return res;
}

// logout best-effort revokes the token server-side, then always clears it
// locally and sends the browser to /login — there's no cookie for the
// server to clean up. Uses plain fetch (not apiFetch): a logout call is
// itself sometimes made with an already-expired token, and shouldn't be
// bounced through apiFetch's own 401 handling first.
export async function logout(): Promise<void> {
  const token = getToken();
  const headers: HeadersInit = token ? { Authorization: "Bearer " + token } : {};

  try {
    await fetch("/api/logout", { method: "POST", headers });
  } catch {
    // best effort
  } finally {
    clearToken();
    window.location.href = "/login";
  }
}
