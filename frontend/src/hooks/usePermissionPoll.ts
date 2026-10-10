import { useEffect, useRef, useState } from "react";
import { apiFetchJSON } from "../api/client";
import { startPolling } from "../lib/poll";

// usePermissionPoll polls url every intervalMs while enabled, otherwise
// reports an empty list — porting loadLoginEvents/loadDownloadEvents'
// "renders as empty rather than surfacing a 403" behavior (dashboard.js:
// 597-625), each gated on its own permission rather than the main
// Promise.all poll's permission.PermissionView. The effect depends on
// `enabled` directly (not just a ref) so that a permission becoming known
// after /api/me resolves triggers an immediate fetch rather than waiting
// for the next scheduled poll. Like usePoll, it pauses in a hidden tab and
// keeps the previous data reference when a response is unchanged.
export function usePermissionPoll<T>(url: string, enabled: boolean, intervalMs = 2000): T[] {
  const [data, setData] = useState<T[]>([]);
  const lastText = useRef<string | null>(null);

  useEffect(() => {
    if (!enabled) return;

    return startPolling(
      (signal) =>
        apiFetchJSON<T[]>(url, { signal })
          .then((d) => {
            if (signal.aborted) return;
            const text = JSON.stringify(d ?? []);
            if (text === lastText.current) return;
            lastText.current = text;
            setData(d || []);
          })
          .catch(() => {}),
      intervalMs,
    );
  }, [url, enabled, intervalMs]);

  // Derived rather than reset inside the effect: a disabled poll reports an
  // empty list without an extra render.
  return enabled ? data : [];
}
