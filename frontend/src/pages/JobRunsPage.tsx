import type { JobRunEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useAuth } from "../auth/useAuth";
import { JobRunLogSection } from "../components/JobRunLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function JobRunsPage() {
  const session = useAuth();
  const events = usePermissionPoll<JobRunEventJSON>("/api/job-runs", session.canViewJobRunLog);

  return (
    <>
      <PageHeader title="Job runs" subtitle="History of completed backup job runs." />
      <RequirePermission test={(s) => s.canViewJobRunLog}>
        <JobRunLogSection events={events} />
      </RequirePermission>
    </>
  );
}
