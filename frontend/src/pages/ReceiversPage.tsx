import type { ReceiverSnapshot } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { useAuth } from "../auth/useAuth";
import { ReceiversSection } from "../components/ReceiversSection";
import { PageHeader } from "../components/PageHeader";

export function ReceiversPage() {
  const session = useAuth();
  const { data: receivers } = usePoll<ReceiverSnapshot>("/api/receivers");

  return (
    <>
      <PageHeader title="Receivers" subtitle="Remote instances sending backups to this one." />
      <ReceiversSection receivers={receivers} canDownload={session.canDownload} />
    </>
  );
}
