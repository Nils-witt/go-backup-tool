import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { NotificationConfigSection } from "../components/NotificationConfigSection";

export function NotificationSettingsPage() {
  return (
    <>
      <PageHeader
        title="Notifications"
        subtitle="Webhook and email destinations used by job failures, receivers, and the report."
      />
      <RequirePermission test={(s) => s.canManageSettings}>
        <NotificationConfigSection />
      </RequirePermission>
    </>
  );
}
