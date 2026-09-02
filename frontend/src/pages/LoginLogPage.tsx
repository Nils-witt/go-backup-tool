import type { LoginEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useSession } from "../context/SessionContext";
import { LoginLogSection } from "../components/LoginLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function LoginLogPage() {
  const session = useSession();
  const events = usePermissionPoll<LoginEventJSON>("/api/login-events", session.canViewLoginLog);

  return (
    <>
      <PageHeader title="Login log" subtitle="Authentication attempts against the web UI." />
      <RequirePermission test={(s) => s.canViewLoginLog}>
        <LoginLogSection events={events} />
      </RequirePermission>
    </>
  );
}
