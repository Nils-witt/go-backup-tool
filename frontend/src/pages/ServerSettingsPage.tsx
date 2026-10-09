import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { ServerConfigSection } from "../components/ServerConfigSection";

export function ServerSettingsPage() {
  return (
    <>
      <PageHeader title="Servers" subtitle="Where jobs upload their backups to." />
      <RequirePermission test={(s) => s.canManageJobs}>
        <ServerConfigSection />
      </RequirePermission>
    </>
  );
}
