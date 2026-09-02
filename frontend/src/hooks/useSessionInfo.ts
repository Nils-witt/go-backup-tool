import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { SessionInfoJSON } from "../api/types";

export interface SessionInfoState {
  info: SessionInfoJSON | null;
  loaded: boolean;
  canDownload: boolean;
  canRetry: boolean;
  canViewLoginLog: boolean;
  canViewDownloadLog: boolean;
}

// useSessionInfo fetches GET /api/session once (a session's own permissions
// don't change without logging in again): the currently authenticated
// session's permissions, used to gate which sections/controls render — the
// server-side handlers behind each one enforce the same rules on every
// actual request, so this is purely a display-time mirror of that, per
// handleSessionInfo's own doc comment in webui.go.
export function useSessionInfo(): SessionInfoState {
  const [info, setInfo] = useState<SessionInfoJSON | null>(null);
  const [loaded, setLoaded] = useState(false);

  const load = useCallback(() => {
    apiFetchJSON<SessionInfoJSON>("/api/session")
      .then((data) => {
        setInfo(data);
        setLoaded(true);
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const permissions = info?.permissions ?? [];

  return {
    info,
    loaded,
    canDownload: permissions.includes("download"),
    // canRetry mirrors the server's own gate on POST /api/jobs/{name}/retry
    // (requireAdmin in webui.go).
    canRetry: loaded && !!info?.admin,
    canViewLoginLog: permissions.includes("login-log"),
    canViewDownloadLog: permissions.includes("download-log"),
  };
}
