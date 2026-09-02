import { IdentitySection } from "../components/IdentitySection";
import { PageHeader } from "../components/PageHeader";

export function IdentityPage() {
  return (
    <>
      <PageHeader
        title="Server identity"
        subtitle="This instance's UUID and public key, for use as a receiver."
      />
      <IdentitySection />
    </>
  );
}
