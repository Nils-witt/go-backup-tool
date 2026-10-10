import { useCallback, useEffect, useRef, useState } from "react";
import { apiFetchJSON } from "../api/client";
import { startPolling } from "../lib/poll";

export interface PollState<T> {
  data: T[];
  loaded: boolean;
  error: string | null;
  // refreshNow re-polls immediately (used after a mutation like "retry
  // failed targets" so its effect shows up right away, rather than waiting
  // up to intervalMs for the next scheduled poll) without resetting the
  // interval's own schedule.
  refreshNow: () => void;
}

// usePoll fetches url immediately and then every intervalMs for as long as
// the calling component stays mounted — each page now polls only the
// resource(s) it actually renders, rather than one combined poll running
// everywhere. With enabled false it doesn't poll at all (pages that follow
// the live status WebSocket only poll while it's down — see useLiveStatus).
// Polling pauses in a hidden tab (see startPolling), and a response
// identical to the previous one keeps the previous data reference, so an
// unchanged poll doesn't re-render the page.
export function usePoll<T>(url: string, intervalMs = 2000, enabled = true): PollState<T> {
  const [data, setData] = useState<T[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const lastText = useRef<string | null>(null);

  const refresh = useCallback(
    (signal?: AbortSignal) =>
      apiFetchJSON<T[]>(url, { signal })
        .then((d) => {
          if (signal?.aborted) return;
          const text = JSON.stringify(d ?? []);
          if (text !== lastText.current) {
            lastText.current = text;
            setData(d || []);
          }
          setLoaded(true);
          setError(null);
        })
        .catch((err: unknown) => {
          if (!signal?.aborted) setError(String(err));
        }),
    [url],
  );

  useEffect(() => {
    if (!enabled) return;

    return startPolling(refresh, intervalMs);
  }, [refresh, intervalMs, enabled]);

  const refreshNow = useCallback(() => void refresh(), [refresh]);

  return { data, loaded, error, refreshNow };
}
