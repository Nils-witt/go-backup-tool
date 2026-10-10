import { useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import type { JobRunEventJSON, JobSnapshot, ReceiverSnapshot, RunState } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useLiveStatus } from "../hooks/useLiveStatus";
import { useMeta } from "../hooks/useMeta";
import { useRemoteStatus, type RemoteStatus } from "../hooks/useRemoteStatus";
import { useAuth } from "../auth/useAuth";
import { JobsGrid } from "../components/JobsGrid";
import { PageHeader } from "../components/PageHeader";
import { ReceiversSection } from "../components/ReceiversSection";
import { RemoteBackendsDialog } from "../components/RemoteBackendsDialog";
import { StatusChip } from "../components/StatusChip";
import { SummaryTile } from "../components/StatusSummary";
import { TopologyChart } from "../components/TopologyChart";
import { countStates, type Sourced } from "../lib/status";
import { useRemoteBackends } from "../lib/remoteBackends";
import { fmtRelative, fmtTime, hasTime } from "../lib/format";
import Button from "@mui/material/Button";
import Grid from "@mui/material/Grid";
import Stack from "@mui/material/Stack";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";

// Run history changes only when a job finishes, so it's polled far less
// often than the live job state.
const runHistoryPollMs = 15000;

// RemoteBackendsBar shows each remote backend's connection state, so one
// that's unreachable or rejecting its token is visible rather than its jobs
// silently missing from the merged view.
function RemoteBackendsBar({ remotes }: { remotes: RemoteStatus[] }) {
  return (
    <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap", mb: 2 }}>
      {remotes.map((r) => (
        <StatusChip
          key={r.backend.id}
          state={r.error ? "failed" : r.loaded ? "ok" : "idle"}
          label={r.name}
          error={r.error ?? undefined}
        />
      ))}
    </Stack>
  );
}

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
  const localJobs = live.live ? live.jobs : poll.data;
  const localReceivers = live.live ? live.receivers : receiverPoll.data;

  const meta = useMeta();
  const [backends] = useRemoteBackends();
  const remotes = useRemoteStatus(backends);
  const [remotesOpen, setRemotesOpen] = useState(false);

  // With remote backends configured, every job and receiver is tagged with
  // the instance it came from; the summary tiles count across all of them.
  const localName = backends.length ? meta?.instanceName || "this instance" : undefined;
  // Memoized so the topology chart's own buildTopology memo (keyed on these
  // arrays) only re-runs when the underlying data changed.
  const jobs = useMemo<Sourced<JobSnapshot>[]>(
    () => [
      ...localJobs.map((j) => ({ ...j, source: localName })),
      ...remotes.flatMap((r) => r.jobs.map((j) => ({ ...j, source: r.name, remote: true }))),
    ],
    [localJobs, remotes, localName],
  );
  const receivers = useMemo<Sourced<ReceiverSnapshot>[]>(
    () => [
      ...localReceivers.map((r) => ({ ...r, source: localName })),
      ...remotes.flatMap((r) =>
        r.receivers.map((rcv) => ({ ...rcv, source: r.name, remote: true })),
      ),
    ],
    [localReceivers, remotes, localName],
  );

  const runs = usePermissionPoll<JobRunEventJSON>(
    "/api/job-runs",
    session.canViewJobRunLog,
    runHistoryPollMs,
  );
  const runsByJob = useMemo(() => {
    if (!session.canViewJobRunLog) return undefined;
    const m = new Map<string, JobRunEventJSON[]>();
    for (const r of runs) {
      const list = m.get(r.job_name);
      if (list) list.push(r);
      else m.set(r.job_name, [r]);
    }
    return m;
  }, [runs, session.canViewJobRunLog]);

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
        action={
          <Button size="small" variant="outlined" onClick={() => setRemotesOpen(true)}>
            Remote backends
          </Button>
        }
      />
      <RemoteBackendsDialog open={remotesOpen} onClose={() => setRemotesOpen(false)} />

      {remotes.length ? <RemoteBackendsBar remotes={remotes} /> : null}

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
                  {nextJob.source ? nextJob.source + " · " : ""}
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
