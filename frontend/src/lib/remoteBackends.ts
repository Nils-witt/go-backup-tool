import { useCallback, useSyncExternalStore } from "react";

// RemoteBackend is another go-backup-tool instance whose status the
// dashboard shows alongside this one's, read with one of its read-only API
// tokens. The remote instance must allow this dashboard's origin in its
// webui.cors-get-origins:.
export interface RemoteBackend {
  id: string;
  // url is the remote instance's base URL (origin, plus any path prefix a
  // reverse proxy mounts it under), without a trailing "/".
  url: string;
  token: string;
  label?: string;
}

// The list lives in this browser's localStorage only — never sent to this
// instance's server — so each user keeps their own.
const storageKey = "gbt.remoteBackends";
const changeEvent = "gbt-remote-backends-changed";

const empty: RemoteBackend[] = [];
let cachedRaw: string | null = null;
let cached: RemoteBackend[] = empty;

function isBackend(v: unknown): v is RemoteBackend {
  const b = v as RemoteBackend;
  return (
    typeof b === "object" &&
    b !== null &&
    typeof b.id === "string" &&
    typeof b.url === "string" &&
    typeof b.token === "string"
  );
}

// loadRemoteBackends reads the stored list; unavailable storage or a
// malformed value reads as empty. The parsed list is cached per raw value so
// useSyncExternalStore sees a stable snapshot.
export function loadRemoteBackends(): RemoteBackend[] {
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(storageKey);
  } catch {
    return empty;
  }

  if (raw === cachedRaw) return cached;
  cachedRaw = raw;

  try {
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    cached = Array.isArray(parsed) ? parsed.filter(isBackend) : empty;
  } catch {
    cached = empty;
  }

  return cached;
}

export function saveRemoteBackends(list: RemoteBackend[]) {
  try {
    localStorage.setItem(storageKey, JSON.stringify(list));
  } catch {
    // Storage unavailable (private window, blocked site data): the change
    // simply doesn't persist.
  }
  window.dispatchEvent(new Event(changeEvent));
}

function subscribe(onChange: () => void): () => void {
  const onStorage = (e: StorageEvent) => {
    if (e.key === storageKey) onChange();
  };
  window.addEventListener("storage", onStorage);
  window.addEventListener(changeEvent, onChange);

  return () => {
    window.removeEventListener("storage", onStorage);
    window.removeEventListener(changeEvent, onChange);
  };
}

// useRemoteBackends returns the stored list and a setter that persists it,
// kept in sync across every component using it and across browser tabs.
export function useRemoteBackends(): [RemoteBackend[], (list: RemoteBackend[]) => void] {
  const list = useSyncExternalStore(subscribe, loadRemoteBackends, () => empty);
  const set = useCallback((next: RemoteBackend[]) => saveRemoteBackends(next), []);

  return [list, set];
}

// normalizeBackendURL validates a user-entered base URL, returning it as
// origin + path without a trailing "/" (or null when it isn't http/https).
export function normalizeBackendURL(input: string): string | null {
  let u: URL;
  try {
    u = new URL(input.trim());
  } catch {
    return null;
  }

  if (u.protocol !== "http:" && u.protocol !== "https:") return null;
  if (u.username || u.password || u.search || u.hash) return null;

  return (u.origin + u.pathname).replace(/\/+$/, "");
}
