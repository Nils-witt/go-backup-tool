import Stack from "@mui/material/Stack";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { ReportConfigSection } from "../components/ReportConfigSection";
import { ReportHistorySection } from "../components/ReportHistorySection";

export function ReportSettingsPage() {
  return (
    <>
      <PageHeader
        title="Report"
        subtitle="A periodic summary of receiver and job activity, sent to your notifications."
      />
      <RequirePermission test={(s) => s.canManageSettings}>
        <Stack spacing={3}>
          <ReportConfigSection />
          <ReportHistorySection />
        </Stack>
      </RequirePermission>
    </>
  );
}
