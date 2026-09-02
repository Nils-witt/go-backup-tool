import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";

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
// everywhere.
export function usePoll<T>(url: string, intervalMs = 2000): PollState<T> {
  const [data, setData] = useState<T[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<T[]>(url)
      .then((d) => {
        setData(d || []);
        setLoaded(true);
        setError(null);
      })
      .catch((err: unknown) => {
        setError(String(err));
      });
  }, [url]);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, intervalMs);
    return () => clearInterval(id);
  }, [refresh, intervalMs]);

  return { data, loaded, error, refreshNow: refresh };
}
