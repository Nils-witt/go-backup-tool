import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type {
  IdentityJSON,
  JobRunEventJSON,
  JobSnapshot,
  PeerSnapshot,
  ReceiverSnapshot,
  RunState,
} from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useLiveStatus } from "../hooks/useLiveStatus";
import { useMeta } from "../hooks/useMeta";
import { useAuth } from "../auth/useAuth";
import { InstanceBar, type Instance } from "../components/InstanceBar";
import { JobsGrid } from "../components/JobsGrid";
import { PageHeader } from "../components/PageHeader";
import { ReceiversSection } from "../components/ReceiversSection";
import { SummaryTile } from "../components/StatusSummary";
import { TopologyChart } from "../components/TopologyChart";
import { countStates, sourcedKey, type Sourced } from "../lib/status";
import { localInstanceId, useHiddenInstances } from "../lib/hiddenInstances";
import { fmtRelative, fmtTime, hasTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Grid from "@mui/material/Grid";
import Stack from "@mui/material/Stack";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";

// Run history changes only when a job finishes, so it's polled far less
// often than the live job state.
const runHistoryPollMs = 15000;

// receiverState folds a receiver's staleness into its state for the summary
// bar: a stale receiver counts as "incomplete", labelled "stale".
function receiverState(r: ReceiverSnapshot): RunState {
  return r.stale ? "incomplete" : r.state;
}

function SectionHeader({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <Stack
      direction="row"
      sx={{ alignItems: "baseline", justifyContent: "space-between", mt: 4, mb: 1.5 }}
    >
      <Typography variant="h6" sx={{ fontWeight: 600 }}>
        {title}
      </Typography>
      {action}
    </Stack>
  );
}

export function DashboardPage() {
  const session = useAuth();
  // The view lives in the URL so a reload or shared link keeps it.
  const [params, setParams] = useSearchParams();
  const view = params.get("view") === "topology" ? "topology" : "overview";
  const live = useLiveStatus();
  // Polling only runs while the live status socket is down.
  const poll = usePoll<JobSnapshot>("/api/status", 2000, !live.live);
  const receiverPoll = usePoll<ReceiverSnapshot>("/api/receivers", 2000, !live.live);
  const peerPoll = usePoll<PeerSnapshot>("/api/peers", 5000, !live.live);
  const localJobs = live.live ? live.jobs : poll.data;
  const localReceivers = live.live ? live.receivers : receiverPoll.data;
  const peers = live.live ? live.peers : peerPoll.data;

  const meta = useMeta();
  const [hidden, setHidden] = useHiddenInstances();
  const [forgetError, setForgetError] = useState<string | null>(null);

  // With other instances pushing their status here, every job and receiver
  // is tagged with the instance it came from; the summary tiles count
  // across all of them.
  const hasPeers = peers.length > 0;
  const localName = hasPeers ? meta?.instanceName || "this instance" : undefined;

  // This instance's server UUID tags its own receivers, so the topology can
  // link another instance's remote target whose server_uuid names this one.
  // Only needed once there are other instances in view.
  const [localUuid, setLocalUuid] = useState<string | undefined>();
  useEffect(() => {
    if (!hasPeers || localUuid) return;
    let cancelled = false;
    apiFetchJSON<IdentityJSON>("/api/identity")
      .then((i) => {
        if (!cancelled) setLocalUuid(i.uuid || undefined);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [hasPeers, localUuid]);

  // Instances hidden with the instance filter are left out of everything
  // below. The filter only applies with other instances in view, so this
  // instance can't end up hidden with no way to show it again.
  const localShown = !hasPeers || !hidden.includes(localInstanceId);
  const shownPeers = useMemo(() => peers.filter((p) => !hidden.includes(p.id)), [peers, hidden]);

  // Memoized so the topology chart's own buildTopology memo (keyed on these
  // arrays) only re-runs when the underlying data changed.
  const jobs = useMemo<Sourced<JobSnapshot>[]>(
    () => [
      ...(localShown ? localJobs.map((j) => ({ ...j, instance: localName })) : []),
      ...shownPeers.flatMap((p) => p.jobs.map((j) => ({ ...j, instance: p.name, remote: true }))),
    ],
    [localJobs, shownPeers, localName, localShown],
  );
  const receivers = useMemo<Sourced<ReceiverSnapshot>[]>(
    () => [
      ...(localShown
        ? localReceivers.map((r) => ({ ...r, instance: localName, instanceUuid: localUuid }))
        : []),
      ...shownPeers.flatMap((p) =>
        p.receivers.map((r) => ({ ...r, instance: p.name, remote: true, instanceUuid: p.id })),
      ),
    ],
    [localReceivers, shownPeers, localName, localUuid, localShown],
  );

  const forget = (p: PeerSnapshot) => {
    setForgetError(null);
    apiFetchOK(
      `/api/peers/${encodeURIComponent(p.id)}`,
      { method: "DELETE" },
      `forgetting ${p.name} failed`,
    )
      .then(() => peerPoll.refreshNow())
      .catch((err: unknown) => setForgetError(err instanceof Error ? err.message : String(err)));
  };

  const instances: Instance[] = [
    { id: localInstanceId, name: localName ?? "this instance", state: "ok" },
    ...peers.map((p): Instance => ({
      id: p.id,
      name: p.name,
      state: p.offline ? "failed" : "ok",
      error: p.offline
        ? `No status pushed since ${fmtTime(p.last_seen)} (it pushes at least every ${p.interval}). Its jobs and receivers show that last status.`
        : undefined,
      onForget: p.offline && session.canManageSettings ? () => forget(p) : undefined,
    })),
  ];

  const runs = usePermissionPoll<JobRunEventJSON>(
    "/api/job-runs",
    session.canViewJobRunLog,
    runHistoryPollMs,
  );
  // With the job run log readable, every job of this instance gets an
  // entry, empty if it hasn't run yet; other instances' jobs show no run
  // history, since their run logs stay on them.
  const runsByJob = useMemo(() => {
    const m = new Map<string, JobRunEventJSON[]>();
    if (!session.canViewJobRunLog || !localShown) return m;

    for (const j of localJobs) m.set(sourcedKey(localName, j.name), []);
    for (const r of runs) m.get(sourcedKey(localName, r.job_name))?.push(r);
    return m;
  }, [runs, session.canViewJobRunLog, localJobs, localName, localShown]);

  const { jobCounts, targetCounts, targets, nextJob } = useMemo(() => {
    const allTargets = jobs.flatMap((j) => j.targets || []);

    // The earliest upcoming run, in one pass rather than a sort.
    let nextJob: Sourced<JobSnapshot> | undefined;
    let nextAt = Infinity;
    for (const j of jobs) {
      if (!hasTime(j.next_run)) continue;
      const at = Date.parse(j.next_run);
      if (at < nextAt) {
        nextAt = at;
        nextJob = j;
      }
    }

    return {
      jobCounts: countStates(jobs.map((j) => j.state)),
      targetCounts: countStates(allTargets.map((t) => t.state)),
      targets: allTargets,
      nextJob,
    };
  }, [jobs]);
  const receiverCounts = useMemo(() => countStates(receivers.map(receiverState)), [receivers]);
  const attention = (jobCounts.failed ?? 0) + (jobCounts.incomplete ?? 0);
  const targetsOk = targetCounts.ok ?? 0;

  return (
    <>
      <PageHeader
        title="Dashboard"
        subtitle="Backup jobs, their recent runs, and the receivers accepting backups from other instances."
      />

      {hasPeers ? (
        <InstanceBar instances={instances} hidden={hidden} setHidden={setHidden} />
      ) : null}
      {forgetError ? (
        <Alert severity="error" sx={{ mb: 2 }} onClose={() => setForgetError(null)}>
          {forgetError}
        </Alert>
      ) : null}

      <Grid container spacing={2} sx={{ mb: 3 }}>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <SummaryTile
            title="Jobs"
            value={jobs.length}
            caption={
              !jobs.length
                ? "none configured"
                : attention
                  ? `${attention} need${attention === 1 ? "s" : ""} attention`
                  : "all healthy"
            }
            counts={jobCounts}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <SummaryTile
            title="Targets"
            value={
              <>
                {targetsOk}
                <Typography component="span" variant="h6" color="text.secondary">
                  {" "}
                  / {targets.length}
                </Typography>
              </>
            }
            caption="succeeded on last run"
            counts={targetCounts}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <SummaryTile
            title="Receivers"
            value={receivers.length}
            caption={
              !receivers.length
                ? "none configured"
                : receiverCounts.incomplete
                  ? `${receiverCounts.incomplete} stale`
                  : "all receiving"
            }
            counts={receiverCounts}
            labels={{ incomplete: "stale" }}
          />
        </Grid>
        <Grid size={{ xs: 12, sm: 6, md: 3 }}>
          <SummaryTile
            title="Next run"
            value={nextJob ? fmtRelative(nextJob.next_run) : "—"}
            caption={
              nextJob ? (
                <>
                  {nextJob.instance ? nextJob.instance + " · " : ""}
                  {nextJob.name} · {fmtTime(nextJob.next_run)}
                </>
              ) : (
                "nothing scheduled"
              )
            }
          />
        </Grid>
      </Grid>

      <ToggleButtonGroup
        size="small"
        exclusive
        value={view}
        onChange={(_, v: string | null) => {
          if (v) setParams(v === "topology" ? { view: v } : {}, { replace: true });
        }}
        aria-label="Dashboard view"
      >
        <ToggleButton value="overview">Overview</ToggleButton>
        <ToggleButton value="topology">Topology</ToggleButton>
      </ToggleButtonGroup>

      {view === "topology" ? (
        <>
          <SectionHeader title="Topology" />
          <TopologyChart jobs={jobs} receivers={receivers} />
        </>
      ) : (
        <>
          <SectionHeader title="Jobs" />
          <JobsGrid
            jobs={jobs}
            canRetry={session.canRetry}
            refreshNow={poll.refreshNow}
            runsByJob={runsByJob}
          />

          {receivers.length ? (
            <>
              <SectionHeader title="Receivers" />
              <ReceiversSection receivers={receivers} canDownload={session.canDownload} />
            </>
          ) : null}
        </>
      )}
    </>
  );
}
