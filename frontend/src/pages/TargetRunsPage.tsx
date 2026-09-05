import type { TargetRunEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useSession } from "../context/SessionContext";
import { TargetRunLogSection } from "../components/TargetRunLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function TargetRunsPage() {
  const session = useSession();
  const events = usePermissionPoll<TargetRunEventJSON>(
    "/api/target-runs",
    session.canViewTargetRunLog,
  );

  return (
    <>
      <PageHeader
        title="Target runs"
        subtitle="History of individual target uploads within each job run."
      />
      <RequirePermission test={(s) => s.canViewTargetRunLog}>
        <TargetRunLogSection events={events} />
      </RequirePermission>
    </>
  );
}
