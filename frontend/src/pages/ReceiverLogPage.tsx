import type { ReceiverEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useAuth } from "../auth/useAuth";
import { ReceiverLogSection } from "../components/ReceiverLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function ReceiverLogPage() {
  const session = useAuth();
  const events = usePermissionPoll<ReceiverEventJSON>(
    "/api/receiver-events",
    session.canViewReceiverLog,
  );

  return (
    <>
      <PageHeader title="Receiver log" subtitle="Every PUT/DELETE request served by a receiver." />
      <RequirePermission test={(s) => s.canViewReceiverLog}>
        <ReceiverLogSection events={events} />
      </RequirePermission>
    </>
  );
}
