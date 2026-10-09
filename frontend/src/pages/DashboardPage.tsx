import type { JobSnapshot } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { useLiveStatus } from "../hooks/useLiveStatus";
import { useAuth } from "../auth/useAuth";
import { JobsGrid } from "../components/JobsGrid";
import { PageHeader } from "../components/PageHeader";

export function DashboardPage() {
  const session = useAuth();
  const live = useLiveStatus();
  // Polling only runs while the live status socket is down.
  const poll = usePoll<JobSnapshot>("/api/status", 2000, !live.live);
  const jobs = live.live ? live.jobs : poll.data;

  return (
    <>
      <PageHeader title="Dashboard" subtitle="Backup jobs and their most recent run." />
      <JobsGrid jobs={jobs} canRetry={session.canRetry} refreshNow={poll.refreshNow} />
    </>
  );
}
