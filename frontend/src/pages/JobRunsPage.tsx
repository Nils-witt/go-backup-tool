import type { JobRunEventJSON } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { JobRunLogSection } from "../components/JobRunLogSection";
import { PageHeader } from "../components/PageHeader";

export function JobRunsPage() {
  const { data: events } = usePoll<JobRunEventJSON>("/api/job-runs");

  return (
    <>
      <PageHeader title="Job runs" subtitle="History of completed backup job runs." />
      <JobRunLogSection events={events} />
    </>
  );
}
