import { logout } from "./api/client";
import { useMeta } from "./hooks/useMeta";
import { useSessionInfo } from "./hooks/useSessionInfo";
import { useStatusPoll } from "./hooks/useStatusPoll";
import { usePermissionPoll } from "./hooks/usePermissionPoll";
import type { DownloadEventJSON, LoginEventJSON } from "./api/types";
import { JobsGrid } from "./components/JobsGrid";
import { ReceiversSection } from "./components/ReceiversSection";
import { LogsViewer } from "./components/LogsViewer";
import { LoginLogSection } from "./components/LoginLogSection";
import { JobRunLogSection } from "./components/JobRunLogSection";
import { TargetRunLogSection } from "./components/TargetRunLogSection";
import { IdentitySection } from "./components/IdentitySection";
import { DownloadLogSection } from "./components/DownloadLogSection";
import { UsersAdminSection } from "./components/UsersAdminSection";
import { OidcUsersAdminSection } from "./components/OidcUsersAdminSection";

export function App() {
  const meta = useMeta();
  const session = useSessionInfo();
  const status = useStatusPoll();
  const loginEvents = usePermissionPoll<LoginEventJSON>("/api/login-events", session.canViewLoginLog);
  const downloadEvents = usePermissionPoll<DownloadEventJSON>("/api/download-events", session.canViewDownloadLog);

  return (
    <>
      <div className="top-bar">
        <h1>go-backup-tool</h1>
        {meta?.auth_enabled ? (
          <a
            className="logout-link"
            href="#"
            onClick={(e) => {
              e.preventDefault();
              logout();
            }}
          >
            Log out
          </a>
        ) : null}
      </div>
      <p className="sub">{status.updatedText}</p>

      <JobsGrid jobs={status.jobs} canRetry={session.canRetry} refreshNow={status.refreshNow} />
      <ReceiversSection receivers={status.receivers} canDownload={session.canDownload} />
      <LogsViewer lines={status.logs} />
      <LoginLogSection events={loginEvents} />
      <JobRunLogSection events={status.jobRuns} />
      <TargetRunLogSection events={status.targetRuns} />
      <IdentitySection />
      <DownloadLogSection events={downloadEvents} />
      <UsersAdminSection visible={session.loaded && !!session.info?.admin} />
      <OidcUsersAdminSection visible={session.loaded && !!session.info?.admin && !!session.info?.oidc_enabled} />

      <p className="footer">
        &copy; {new Date().getFullYear()} Witt, Nils · Backup-Tool ·{" "}
        <a href="https://github.com/Nils-witt/go-backup-tool">GitHub</a> · {meta?.version ?? ""} · {meta?.commit ?? ""}
      </p>
    </>
  );
}
