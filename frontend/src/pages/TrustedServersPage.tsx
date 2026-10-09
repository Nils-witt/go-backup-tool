import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { TrustedServersSection } from "../components/TrustedServersSection";

export function TrustedServersPage() {
  return (
    <>
      <PageHeader
        title="Trusted servers"
        subtitle="Remote instances, by server ID and public key, that receivers may accept backups from."
      />
      <RequirePermission test={(s) => s.canManageReceivers}>
        <TrustedServersSection />
      </RequirePermission>
    </>
  );
}
