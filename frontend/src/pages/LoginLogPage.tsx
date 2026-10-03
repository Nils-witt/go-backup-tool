import type { LoginEventJSON } from "../api/types";
import { usePermissionPoll } from "../hooks/usePermissionPoll";
import { useAuth } from "../auth/useAuth";
import { LoginLogSection } from "../components/LoginLogSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function LoginLogPage() {
  const session = useAuth();
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
