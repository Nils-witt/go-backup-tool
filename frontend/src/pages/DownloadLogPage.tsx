import type { DownloadEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useSession } from "../context/SessionContext";
import { DownloadLogSection } from "../components/DownloadLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function DownloadLogPage() {
  const session = useSession();
  const events = usePermissionPoll<DownloadEventJSON>(
    "/api/download-events",
    session.canViewDownloadLog,
  );

  return (
    <>
      <PageHeader title="Download log" subtitle="File downloads served from receivers." />
      <RequirePermission test={(s) => s.canViewDownloadLog}>
        <DownloadLogSection events={events} />
      </RequirePermission>
    </>
  );
}
