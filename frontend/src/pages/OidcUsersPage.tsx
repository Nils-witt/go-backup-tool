import { OidcUsersAdminSection } from "../components/OidcUsersAdminSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function OidcUsersPage() {
  return (
    <>
      <PageHeader title="OIDC users" subtitle="Per-identity permission overrides for SSO logins." />
      <RequirePermission test={(s) => !!s.info?.admin && !!s.info?.oidc_enabled}>
        <OidcUsersAdminSection />
      </RequirePermission>
    </>
  );
}
