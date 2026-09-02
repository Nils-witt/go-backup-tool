import { usePoll } from "../hooks/usePoll";
import { LogsViewer } from "../components/LogsViewer";
import { PageHeader } from "../components/PageHeader";

export function LiveLogsPage() {
  const { data: lines } = usePoll<string>("/api/logs");

  return (
    <>
      <PageHeader
        title="Live logs"
        subtitle="Recent process output, refreshed every couple of seconds."
      />
      <LogsViewer lines={lines} />
    </>
  );
}
