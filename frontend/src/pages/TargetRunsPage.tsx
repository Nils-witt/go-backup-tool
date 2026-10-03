import type { TargetRunEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useAuth } from "../auth/useAuth";
import { TargetRunLogSection } from "../components/TargetRunLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function TargetRunsPage() {
  const session = useAuth();
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
