import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { LogRetentionConfigSection } from "../components/LogRetentionConfigSection";

export function LogRetentionSettingsPage() {
  return (
    <>
      <PageHeader
        title="Log retention"
        subtitle="How long the job run and target run logs keep their history."
      />
      <RequirePermission test={(s) => s.canManageSettings}>
        <LogRetentionConfigSection />
      </RequirePermission>
    </>
  );
}
