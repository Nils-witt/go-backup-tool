import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { ReportConfigSection } from "../components/ReportConfigSection";

export function ReportSettingsPage() {
  return (
    <>
      <PageHeader
        title="Report"
        subtitle="A periodic summary of receiver and job activity, sent to your notifications."
      />
      <RequirePermission test={(s) => s.canManageSettings}>
        <ReportConfigSection />
      </RequirePermission>
    </>
  );
}
