import type { ReceiverSnapshot } from "../api/types";
import { usePoll } from "../hooks/usePoll";
import { useLiveStatus } from "../hooks/useLiveStatus";
import { useAuth } from "../auth/useAuth";
import { ReceiversSection } from "../components/ReceiversSection";
import { PageHeader } from "../components/PageHeader";

export function ReceiversPage() {
  const session = useAuth();
  const live = useLiveStatus();
  // Polling only runs while the live status socket is down.
  const poll = usePoll<ReceiverSnapshot>("/api/receivers", 2000, !live.live);
  const receivers = live.live ? live.receivers : poll.data;

  return (
    <>
      <PageHeader title="Receivers" subtitle="Remote instances sending backups to this one." />
      <ReceiversSection receivers={receivers} canDownload={session.canDownload} />
    </>
  );
}
