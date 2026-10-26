import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { StatusPushConfigSection } from "../components/StatusPushConfigSection";

export function StatusPushSettingsPage() {
  return (
    <>
      <PageHeader
        title="Status push"
        subtitle="Show this instance's jobs and receivers on a central instance's dashboard."
      />
      <RequirePermission test={(s) => s.canManageSettings}>
        <StatusPushConfigSection />
      </RequirePermission>
    </>
  );
}
