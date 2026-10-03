import { useEffect, useState } from "react";
import { fetchMeta } from "../api/client";
import type { MetaJSON } from "../api/types";

// useMeta fetches GET /api/meta once: build/version info for the footer.
// Always public/unauthenticated, since the footer is shown on the login
// page too.
export function useMeta(): MetaJSON | null {
  const [meta, setMeta] = useState<MetaJSON | null>(null);

  useEffect(() => {
    let cancelled = false;

    fetchMeta()
      .then((data) => {
        if (!cancelled) setMeta(data);
      })
      .catch(() => {});

    return () => {
      cancelled = true;
    };
  }, []);

  return meta;
}
