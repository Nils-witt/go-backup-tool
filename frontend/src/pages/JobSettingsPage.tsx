import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { JobConfigSection } from "../components/JobConfigSection";

export function JobSettingsPage() {
  return (
    <>
      <PageHeader title="Jobs" subtitle="What this instance backs up, when, and where to." />
      <RequirePermission test={(s) => s.canManageJobs}>
        <JobConfigSection />
      </RequirePermission>
    </>
  );
}
