import { Route, Routes } from "react-router-dom";
import { SessionProvider } from "./context/SessionContext";
import { Layout } from "./Layout";
import { DashboardPage } from "./pages/DashboardPage";
import { ReceiversPage } from "./pages/ReceiversPage";
import { LiveLogsPage } from "./pages/LiveLogsPage";
import { JobRunsPage } from "./pages/JobRunsPage";
import { TargetRunsPage } from "./pages/TargetRunsPage";
import { LoginLogPage } from "./pages/LoginLogPage";
import { DownloadLogPage } from "./pages/DownloadLogPage";
import { IdentityPage } from "./pages/IdentityPage";
import { UsersPage } from "./pages/UsersPage";

export function App() {
  return (
    <SessionProvider>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<DashboardPage />} />
          <Route path="receivers" element={<ReceiversPage />} />
          <Route path="identity" element={<IdentityPage />} />
          <Route path="logs" element={<LiveLogsPage />} />
          <Route path="logs/job-runs" element={<JobRunsPage />} />
          <Route path="logs/target-runs" element={<TargetRunsPage />} />
          <Route path="logs/login" element={<LoginLogPage />} />
          <Route path="logs/downloads" element={<DownloadLogPage />} />
          <Route path="admin/users" element={<UsersPage />} />
        </Route>
      </Routes>
    </SessionProvider>
  );
}
