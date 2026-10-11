import { useCallback, useSyncExternalStore } from "react";

// localInstanceId stands for this instance in the hidden list; remote
// backends are listed by their RemoteBackend id.
export const localInstanceId = "local";

// Which instances the dashboard hides lives in this browser's localStorage,
// like the remote backend list it filters (see lib/remoteBackends).
const storageKey = "gbt.hiddenInstances";
const changeEvent = "gbt-hidden-instances-changed";

const empty: string[] = [];
let cachedRaw: string | null = null;
let cached: string[] = empty;

// loadHiddenInstances reads the stored ids; unavailable storage or a
// malformed value reads as none hidden. The parsed list is cached per raw
// value so useSyncExternalStore sees a stable snapshot.
function loadHiddenInstances(): string[] {
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
    cached = Array.isArray(parsed)
      ? parsed.filter((v): v is string => typeof v === "string")
      : empty;
  } catch {
    cached = empty;
  }

  return cached;
}

function saveHiddenInstances(ids: string[]) {
  try {
    localStorage.setItem(storageKey, JSON.stringify(ids));
  } catch {
    // Storage unavailable: the change simply doesn't persist.
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

// useHiddenInstances returns the ids of the instances hidden from the
// dashboard and a setter that persists them, kept in sync across tabs.
export function useHiddenInstances(): [string[], (ids: string[]) => void] {
  const ids = useSyncExternalStore(subscribe, loadHiddenInstances, () => empty);
  const set = useCallback((next: string[]) => saveHiddenInstances(next), []);

  return [ids, set];
}
