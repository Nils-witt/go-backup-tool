import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { GPGKeysSection } from "../components/GPGKeysSection";

export function GPGKeysPage() {
  return (
    <>
      <PageHeader title="GPG keys" subtitle="Public keys backups are encrypted to." />
      <RequirePermission test={(s) => s.canManageJobs}>
        <GPGKeysSection />
      </RequirePermission>
    </>
  );
}
