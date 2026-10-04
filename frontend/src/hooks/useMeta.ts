import { useEffect, useState } from "react";
import { fetchMeta } from "../api/client";
import type { MetaJSON } from "../api/types";

// metaPromise is shared by every useMeta caller (footer, app bar, login
// card), so GET /api/meta is only requested once per page load.
let metaPromise: Promise<MetaJSON> | null = null;

// useMeta fetches GET /api/meta once: build/version info for the footer and
// the optional instance name. Always public/unauthenticated, since the
// footer and instance name are shown on the login page too.
export function useMeta(): MetaJSON | null {
  const [meta, setMeta] = useState<MetaJSON | null>(null);

  useEffect(() => {
    let cancelled = false;

    if (!metaPromise) {
      metaPromise = fetchMeta();
      metaPromise.catch(() => {
        metaPromise = null;
      });
    }

    metaPromise
      .then((data) => {
        if (!cancelled) setMeta(data);
      })
      .catch(() => {});

    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (meta?.instanceName) document.title = `${meta.instanceName} · go-backup-tool`;
  }, [meta?.instanceName]);

  return meta;
}
