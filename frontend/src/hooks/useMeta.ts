import { useEffect, useState } from "react";
import type { MetaJSON } from "../api/types";

// useMeta fetches GET /api/meta once: build/version info and whether auth
// is enabled at all, for the footer and the "Log out" link's visibility.
// Always-public/unauthenticated — fetched with plain fetch(), not
// apiFetch(), since it must render before a session exists.
export function useMeta(): MetaJSON | null {
  const [meta, setMeta] = useState<MetaJSON | null>(null);

  useEffect(() => {
    let cancelled = false;

    fetch("/api/meta")
      .then((r) => r.json())
      .then((data: MetaJSON) => {
        if (!cancelled) setMeta(data);
      })
      .catch(() => {});

    return () => {
      cancelled = true;
    };
  }, []);

  return meta;
}
