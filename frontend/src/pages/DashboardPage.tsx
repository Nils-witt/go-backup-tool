import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { apiFetchJSON } from "../api/client";
import type {
  IdentityJSON,
  JobRunEventJSON,
  JobSnapshot,
  ReceiverSnapshot,
  RunState,
} from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useLiveStatus } from "../hooks/useLiveStatus";
import { useMeta } from "../hooks/useMeta";
import { useRemoteStatus } from "../hooks/useRemoteStatus";
import { useAuth } from "../auth/useAuth";
import { JobsGrid } from "../components/JobsGrid";
import { PageHeader } from "../components/PageHeader";
import { ReceiversSection } from "../components/ReceiversSection";
import { RemoteBackendsDialog } from "../components/RemoteBackendsDialog";
import { SummaryTile } from "../components/StatusSummary";
import { TopologyChart } from "../components/TopologyChart";
import { countStates, sourcedKey, type Sourced } from "../lib/status";
import { useRemoteBackends } from "../lib/remoteBackends";
import { localInstanceId, useHiddenInstances } from "../lib/hiddenInstances";
import { fmtRelative, fmtTime, hasTime } from "../lib/format";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import ErrorOutlineIcon from "@mui/icons-material/ErrorOutlineOutlined";
import VisibilityOffOutlinedIcon from "@mui/icons-material/VisibilityOffOutlined";
import VisibilityOutlinedIcon from "@mui/icons-material/VisibilityOutlined";
import Grid from "@mui/material/Grid";
import Popover from "@mui/material/Popover";
import Stack from "@mui/material/Stack";
import ToggleButton from "@mui/material/ToggleButton";
import ToggleButtonGroup from "@mui/material/ToggleButtonGroup";
import Typography from "@mui/material/Typography";
import { alpha, useTheme } from "@mui/material/styles";

// Run history changes only when a job finishes, so it's polled far less
// often than the live job state.
const runHistoryPollMs = 15000;

interface Instance {
  id: string;
  name: string;
  state: RunState;
  error?: string;
}

// InstanceChip shows one instance's connection state, so a remote that's
// unreachable or rejecting its token is visible rather than its jobs silently
// missing from the merged view. Clicking it shows or hides the instance's
// jobs and receivers; an error icon opens the error text.
function InstanceChip({
  instance,
  shown,
  onToggle,
}: {
  instance: Instance;
  shown: boolean;
  onToggle: () => void;
}) {
  const theme = useTheme();
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const color = shown ? theme.palette.status[instance.state] : theme.palette.text.disabled;
  const tint = (a: number) => alpha(color, theme.palette.mode === "dark" ? a + 0.06 : a);
  const VisibilityIcon = shown ? VisibilityOutlinedIcon : VisibilityOffOutlinedIcon;

  return (
    <>
      <Chip
        size="small"
        label={instance.name}
        icon={<VisibilityIcon />}
        variant={shown ? "filled" : "outlined"}
        onClick={onToggle}
        aria-pressed={shown}
        aria-label={`${shown ? "Hide" : "Show"} ${instance.name}`}
        onDelete={instance.error ? (e) => setAnchor(e.currentTarget.parentElement) : undefined}
        deleteIcon={
          instance.error ? (
            <ErrorOutlineIcon aria-label={`${instance.name}: show error details`} />
          ) : undefined
        }
        sx={{
          color,
          borderColor: color,
          backgroundColor: shown ? tint(0.12) : "transparent",
          fontWeight: 600,
          fontSize: ".72rem",
          textTransform: "uppercase",
          letterSpacing: ".02em",
          textDecoration: shown ? undefined : "line-through",
          "& .MuiChip-icon, & .MuiChip-deleteIcon": { color, fontSize: "1rem" },
          "& .MuiChip-deleteIcon:hover": { color },
          "&.MuiChip-clickable:hover": { backgroundColor: tint(0.2) },
        }}
      />
      {instance.error ? (
        <Popover
          open={anchor !== null}
          anchorEl={anchor}
          onClose={() => setAnchor(null)}
          anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
          transformOrigin={{ vertical: "top", horizontal: "right" }}
        >
          <Typography
            variant="body2"
            color="error"
            sx={{ p: 1.5, maxWidth: 400, overflowWrap: "anywhere", whiteSpace: "pre-wrap" }}
          >
            {instance.error}
          </Typography>
        </Popover>
      ) : null}
    </>
  );
}

// InstanceBar lists every instance in view as an InstanceChip. At least one
// stays shown: hiding the last is ignored.
function InstanceBar({
  instances,
  hidden,
  setHidden,
}: {
  instances: Instance[];
  hidden: string[];
  setHidden: (ids: string[]) => void;
}) {
  const shownCount = instances.filter((i) => !hidden.includes(i.id)).length;
  const toggle = (id: string) => {
    if (hidden.includes(id)) setHidden(hidden.filter((h) => h !== id));
    // Ids of backends no longer configured stay in the list, so re-adding
    // one doesn't change whether it's hidden.
    else if (shownCount > 1) setHidden([...hidden, id]);
  };

  return (
    <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap", mb: 2 }}>
      {instances.map((i) => (
        <InstanceChip
          key={i.id}
          instance={i}
          shown={!hidden.includes(i.id)}
          onToggle={() => toggle(i.id)}
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
  const [hidden, setHidden] = useHiddenInstances();

  // With remote backends configured, every job and receiver is tagged with
  // the instance it came from; the summary tiles count across all of them.
  const localName = backends.length ? meta?.instanceName || "this instance" : undefined;

  // This instance's server UUID tags its own receivers, so the topology can
  // link a remote target whose server_uuid names this instance. Only needed
  // once there are other instances in view.
  const [localUuid, setLocalUuid] = useState<string | undefined>();
  const hasBackends = backends.length > 0;
  useEffect(() => {
    if (!hasBackends || localUuid) return;
    let cancelled = false;
    apiFetchJSON<IdentityJSON>("/api/identity")
      .then((i) => {
        if (!cancelled) setLocalUuid(i.uuid || undefined);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [hasBackends, localUuid]);
  // Instances hidden with the instance filter are left out of everything
  // below. The filter only applies with remote backends configured, so this
  // instance can't end up hidden with no way to show it again.
  const localShown = !hasBackends || !hidden.includes(localInstanceId);
  const shownRemotes = useMemo(
    () => remotes.filter((r) => !hidden.includes(r.backend.id)),
    [remotes, hidden],
  );

  // Memoized so the topology chart's own buildTopology memo (keyed on these
  // arrays) only re-runs when the underlying data changed.
  const jobs = useMemo<Sourced<JobSnapshot>[]>(
    () => [
      ...(localShown ? localJobs.map((j) => ({ ...j, source: localName })) : []),
      ...shownRemotes.flatMap((r) => r.jobs.map((j) => ({ ...j, source: r.name, remote: true }))),
    ],
    [localJobs, shownRemotes, localName, localShown],
  );
  const receivers = useMemo<Sourced<ReceiverSnapshot>[]>(
    () => [
      ...(localShown
        ? localReceivers.map((r) => ({ ...r, source: localName, instanceUuid: localUuid }))
        : []),
      ...shownRemotes.flatMap((r) =>
        r.receivers.map((rcv) => ({ ...rcv, source: r.name, remote: true, instanceUuid: r.uuid })),
      ),
    ],
    [localReceivers, shownRemotes, localName, localUuid, localShown],
  );

  const runs = usePermissionPoll<JobRunEventJSON>(
    "/api/job-runs",
    session.canViewJobRunLog,
    runHistoryPollMs,
  );
  // Every job whose instance's run log is readable gets an entry, empty if
  // it hasn't run yet; the rest show no run history.
  const runsByJob = useMemo(() => {
    const m = new Map<string, JobRunEventJSON[]>();
    const readable = new Set<string | undefined>();
    const add = (source: string | undefined, list: JobRunEventJSON[] | undefined) => {
      if (!list) return;
      readable.add(source);
      for (const r of list) {
        const key = sourcedKey(source, r.job_name);
        const existing = m.get(key);
        if (existing) existing.push(r);
        else m.set(key, [r]);
      }
    };
    if (localShown) add(localName, session.canViewJobRunLog ? runs : undefined);
    for (const r of shownRemotes) add(r.name, r.runs);

    for (const j of jobs) {
      const key = sourcedKey(j.source, j.name);
      if (readable.has(j.source) && !m.has(key)) m.set(key, []);
    }
    return m;
  }, [runs, session.canViewJobRunLog, shownRemotes, localName, localShown, jobs]);

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

      {remotes.length ? (
        <InstanceBar
          instances={[
            {
              id: localInstanceId,
              name: localName ?? "this instance",
              state:
                live.live || (poll.loaded && !poll.error) ? "ok" : poll.error ? "failed" : "idle",
              error: live.live ? undefined : (poll.error ?? undefined),
            },
            ...remotes.map((r) => ({
              id: r.backend.id,
              name: r.name,
              state: (r.error ? "failed" : r.loaded ? "ok" : "idle") as RunState,
              error: r.error ?? undefined,
            })),
          ]}
          hidden={hidden}
          setHidden={setHidden}
        />
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
