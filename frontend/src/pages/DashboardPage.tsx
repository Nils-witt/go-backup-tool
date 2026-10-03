import type { JobSnapshot } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { useAuth } from "../auth/useAuth";
import { JobsGrid } from "../components/JobsGrid";
import { PageHeader } from "../components/PageHeader";

export function DashboardPage() {
  const session = useAuth();
  const { data: jobs, refreshNow } = usePoll<JobSnapshot>("/api/status");

  return (
    <>
      <PageHeader title="Dashboard" subtitle="Backup jobs and their most recent run." />
      <JobsGrid jobs={jobs} canRetry={session.canRetry} refreshNow={refreshNow} />
    </>
  );
}
