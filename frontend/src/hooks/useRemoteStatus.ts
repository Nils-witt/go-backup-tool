import { useEffect, useMemo, useState } from "react";
import { ApiError, fetchRemoteMeta, remoteErrorMessage, remoteFetchJSON } from "../api/client";
import type { JobRunEventJSON, JobSnapshot, ReceiverSnapshot } from "../api/types";
import type { RemoteBackend } from "../lib/remoteBackends";

// Remote instances are polled, not followed over their live status
// WebSocket: opening one needs POST /api/live/ticket, and a remote's CORS
// (webui.cors-get-origins:) only ever allows GET.
const remotePollMs = 5000;
// Run history changes only when a job finishes (see the dashboard's
// runHistoryPollMs).
const remoteRunsPollMs = 15000;

export interface RemoteStatus {
  backend: RemoteBackend;
  // name is what the dashboard tags this backend's jobs/receivers with: its
  // label, else its own instance name, else its host.
  name: string;
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
  // runs is the backend's job run log, newest first; undefined until it
  // loads, or when its token can't read it (a remote older than API tokens
  // carrying the job-run-log permission).
  runs?: JobRunEventJSON[];
  loaded: boolean;
  error: string | null;
}

interface Polled {
  instanceName?: string;
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
  runs?: JobRunEventJSON[];
  loaded: boolean;
  error: string | null;
}

const initial: Polled = { jobs: [], receivers: [], loaded: false, error: null };

function displayName(b: RemoteBackend, instanceName?: string): string {
  if (b.label) return b.label;
  if (instanceName) return instanceName;
  try {
    return new URL(b.url).host;
  } catch {
    return b.url;
  }
}

// useRemoteStatus polls each backend's GET /api/status and /api/receivers
// every remotePollMs, its /api/job-runs every remoteRunsPollMs, and its
// /api/meta once for its instance name. A failing backend keeps its last
// good data alongside the error.
export function useRemoteStatus(backends: RemoteBackend[]): RemoteStatus[] {
  const [polled, setPolled] = useState<Record<string, Polled>>({});

  useEffect(() => {
    let cancelled = false;
    const update = (id: string, patch: Partial<Polled>) => {
      if (cancelled) return;
      setPolled((prev) => ({ ...prev, [id]: { ...initial, ...prev[id], ...patch } }));
    };

    const refresh = (b: RemoteBackend) => {
      Promise.all([
        remoteFetchJSON<JobSnapshot[]>(b, "/api/status"),
        remoteFetchJSON<ReceiverSnapshot[]>(b, "/api/receivers"),
      ])
        .then(([jobs, receivers]) =>
          update(b.id, { jobs: jobs || [], receivers: receivers || [], loaded: true, error: null }),
        )
        .catch((err: unknown) => update(b.id, { error: remoteErrorMessage(err) }));
    };

    // Run history is optional: a failure here never marks the backend as
    // errored. A 403 means its token can't read the log, so the history is
    // hidden; any other failure keeps the last good runs, as above.
    const refreshRuns = (b: RemoteBackend) => {
      remoteFetchJSON<JobRunEventJSON[]>(b, "/api/job-runs")
        .then((runs) => update(b.id, { runs: runs || [] }))
        .catch((err: unknown) => {
          if (err instanceof ApiError && err.status === 403) update(b.id, { runs: undefined });
        });
    };

    for (const b of backends) {
      fetchRemoteMeta(b)
        .then((m) => update(b.id, { instanceName: m.instanceName }))
        .catch(() => {});
      refresh(b);
      refreshRuns(b);
    }

    const id = setInterval(() => backends.forEach(refresh), remotePollMs);
    const runsId = setInterval(() => backends.forEach(refreshRuns), remoteRunsPollMs);
    return () => {
      cancelled = true;
      clearInterval(id);
      clearInterval(runsId);
    };
  }, [backends]);

  // Memoized so callers (the dashboard's job/receiver merge, and through it
  // the topology chart) only recompute when a poll actually lands.
  return useMemo(
    () =>
      backends.map((b) => {
        const p = polled[b.id] ?? initial;
        return {
          backend: b,
          name: displayName(b, p.instanceName),
          jobs: p.jobs,
          receivers: p.receivers,
          runs: p.runs,
          loaded: p.loaded,
          error: p.error,
        };
      }),
    [backends, polled],
  );
}
