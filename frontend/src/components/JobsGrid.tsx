import { useState } from "react";
import { apiFetch } from "../api/client";
import type { JobSnapshot } from "../api/types";
import { Badge } from "./Badge";
import { ConfirmDialog } from "./Dialog";
import { fmtTime, hasTime } from "../lib/format";

interface JobsGridProps {
  jobs: JobSnapshot[];
  canRetry: boolean;
  refreshNow: () => void;
}

export function JobsGrid({ jobs, canRetry, refreshNow }: JobsGridProps) {
  const [pendingRetry, setPendingRetry] = useState<string | null>(null);

  function startRetry(name: string) {
    apiFetch("/api/jobs/" + encodeURIComponent(name) + "/retry", { method: "POST" })
      .then(() => refreshNow())
      .catch(() => {});
  }

  if (!jobs.length) {
    return <p className="empty">no jobs configured</p>;
  }

  return (
    <>
      <div className="grid" id="jobs">
        {jobs.map((j) => {
          const hasFailedTarget = (j.targets || []).some((t) => t.state === "failed");
          const interval = j.interval ? "every " + j.interval : "runs once";
          const duration = j.duration ? " · took " + j.duration : "";
          const size = j.size ? " · " + j.size : "";
          const nextRun = hasTime(j.next_run) ? " · next run: " + fmtTime(j.next_run) : "";

          return (
            <div className="card" key={j.name}>
              <div className="card-head">
                <span className="job-name">{j.name}</span>
                <Badge state={j.state} />
              </div>
              <div className="meta">
                {interval} · last run: {fmtTime(j.last_start)}
                {duration}
                {size}
                {nextRun}
              </div>
              {j.error ? <p className="err">{j.error}</p> : null}
              <ul className="targets">
                {(j.targets || []).map((t, i) => (
                  <li key={i}>
                    <span className="target-name">
                      {t.server} / {t.bucket} <span className="kind">({t.kind})</span>
                      {t.error ? <p className="err">{t.error}</p> : null}
                    </span>
                    <Badge state={t.state} />
                  </li>
                ))}
              </ul>
              {hasFailedTarget && canRetry ? (
                <button
                  type="button"
                  className="retry-btn"
                  onClick={() => setPendingRetry(j.name)}
                >
                  Retry failed targets
                </button>
              ) : null}
            </div>
          );
        })}
      </div>

      <ConfirmDialog
        open={pendingRetry !== null}
        message={
          <>
            Retry failed targets for <strong>{pendingRetry}</strong>?
          </>
        }
        confirmLabel="Retry"
        onConfirm={() => {
          if (pendingRetry) startRetry(pendingRetry);
          setPendingRetry(null);
        }}
        onCancel={() => setPendingRetry(null)}
      />
    </>
  );
}
