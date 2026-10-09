import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { ReceiverConfigSection } from "../components/ReceiverConfigSection";

export function ReceiverSettingsPage() {
  return (
    <>
      <PageHeader
        title="Receiver settings"
        subtitle="Which remote instances may send backups to this one, and where they're stored."
      />
      <RequirePermission test={(s) => s.canManageReceivers}>
        <ReceiverConfigSection />
      </RequirePermission>
    </>
  );
}
