import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";
import { TokensSection } from "../components/TokensSection";

export function TokensPage() {
  return (
    <>
      <PageHeader
        title="API tokens"
        subtitle="Long-lived, revocable read-only tokens for scripts and monitoring."
      />
      <RequirePermission test={(s) => s.canManageTokens}>
        <TokensSection />
      </RequirePermission>
    </>
  );
}
