import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { CommandConfigSection } from "../components/CommandConfigSection";

export function CommandSettingsPage() {
  return (
    <>
      <PageHeader
        title="Commands"
        subtitle="Shell commands job targets run when uploads keep failing or recover."
      />
      <RequirePermission test={(s) => s.canManageJobs}>
        <CommandConfigSection />
      </RequirePermission>
    </>
  );
}
