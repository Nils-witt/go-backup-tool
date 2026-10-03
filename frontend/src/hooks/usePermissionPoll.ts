import { useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";

// usePermissionPoll polls url every intervalMs while enabled, otherwise
// reports an empty list — porting loadLoginEvents/loadDownloadEvents'
// "renders as empty rather than surfacing a 403" behavior (dashboard.js:
// 597-625), each gated on its own permission rather than the main
// Promise.all poll's permission.PermissionView. The effect depends on
// `enabled` directly (not just a ref) so that a permission becoming known
// after /api/me resolves triggers an immediate fetch rather than waiting
// for the next scheduled poll.
export function usePermissionPoll<T>(url: string, enabled: boolean, intervalMs = 2000): T[] {
  const [data, setData] = useState<T[]>([]);

  useEffect(() => {
    if (!enabled) {
      setData([]);
      return;
    }

    let cancelled = false;

    function tick() {
      apiFetchJSON<T[]>(url)
        .then((d) => {
          if (!cancelled) setData(d || []);
        })
        .catch(() => {});
    }

    tick();
    const id = setInterval(tick, intervalMs);

    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [url, enabled, intervalMs]);

  return data;
}
