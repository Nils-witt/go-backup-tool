import type { AuditEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useAuth } from "../auth/useAuth";
import { AuditLogSection } from "../components/AuditLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function AuditLogPage() {
  const session = useAuth();
  const events = usePermissionPoll<AuditEventJSON>("/api/audit-events", session.canViewAuditLog);

  return (
    <>
      <PageHeader title="Audit log" subtitle="Changes users made through the web UI." />
      <RequirePermission test={(s) => s.canViewAuditLog}>
        <AuditLogSection events={events} />
      </RequirePermission>
    </>
  );
}
