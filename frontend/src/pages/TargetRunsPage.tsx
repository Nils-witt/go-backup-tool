import type { TargetRunEventJSON } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { TargetRunLogSection } from "../components/TargetRunLogSection";
import { PageHeader } from "../components/PageHeader";

export function TargetRunsPage() {
  const { data: events } = usePoll<TargetRunEventJSON>("/api/target-runs");

  return (
    <>
      <PageHeader
        title="Target runs"
        subtitle="History of individual target uploads within each job run."
      />
      <TargetRunLogSection events={events} />
    </>
  );
}
