import { UsersAdminSection } from "../components/UsersAdminSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function UsersPage() {
  return (
    <>
      <PageHeader title="Users" subtitle="Web UI accounts, their permissions, and API tokens." />
      <RequirePermission test={(s) => !!s.info?.admin}>
        <UsersAdminSection />
      </RequirePermission>
    </>
  );
}
