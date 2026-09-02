import { useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { IdentityJSON } from "../api/types";

// IdentitySection ports loadIdentity/renderIdentity (dashboard.js:518-548):
// fetched once (this data never changes while the process is running).
export function IdentitySection() {
  const [identity, setIdentity] = useState<IdentityJSON | null>(null);

  useEffect(() => {
    let cancelled = false;

    apiFetchJSON<IdentityJSON>("/api/identity")
      .then((data) => {
        if (!cancelled) setIdentity(data);
      })
      .catch(() => {});

    return () => {
      cancelled = true;
    };
  }, []);

  if (!identity || !identity.uuid) return null;

  return (
    <div id="identity-wrap">
      <h2 className="section-title">Server identity</h2>
      <div className="card identity-card">
        <div className="meta">
          UUID: <code>{identity.uuid}</code>
        </div>
        <div className="meta">
          Public key — paste into a receiving instance's <code>receivers:</code> entry as <code>public-key:</code>:
        </div>
        <pre className="pubkey">{identity.public_key}</pre>
      </div>
    </div>
  );
}
