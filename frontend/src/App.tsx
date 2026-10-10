import { lazy, useEffect } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { AuthProvider } from "./auth/AuthContext";
import { useAuth } from "./auth/useAuth";
import { SSO_CALLBACK_PATH } from "./auth/oidc";
import { Layout } from "./Layout";
import { LoginPage } from "./pages/LoginPage";
import { SsoCallbackPage } from "./pages/SsoCallbackPage";
import { DashboardPage } from "./pages/DashboardPage";

// Every page but the dashboard and the login flow is loaded on demand, so
// the first paint only downloads what it shows; Layout wraps its Outlet in
// a Suspense boundary for the brief chunk fetch on first navigation.
const LiveLogsPage = lazy(() =>
  import("./pages/LiveLogsPage").then((m) => ({ default: m.LiveLogsPage })),
);
const JobRunsPage = lazy(() =>
  import("./pages/JobRunsPage").then((m) => ({ default: m.JobRunsPage })),
);
const TargetRunsPage = lazy(() =>
  import("./pages/TargetRunsPage").then((m) => ({ default: m.TargetRunsPage })),
);
const LoginLogPage = lazy(() =>
  import("./pages/LoginLogPage").then((m) => ({ default: m.LoginLogPage })),
);
const DownloadLogPage = lazy(() =>
  import("./pages/DownloadLogPage").then((m) => ({ default: m.DownloadLogPage })),
);
const ReceiverLogPage = lazy(() =>
  import("./pages/ReceiverLogPage").then((m) => ({ default: m.ReceiverLogPage })),
);
const AuditLogPage = lazy(() =>
  import("./pages/AuditLogPage").then((m) => ({ default: m.AuditLogPage })),
);
const IdentityPage = lazy(() =>
  import("./pages/IdentityPage").then((m) => ({ default: m.IdentityPage })),
);
const TokensPage = lazy(() =>
  import("./pages/TokensPage").then((m) => ({ default: m.TokensPage })),
);
const ReceiverSettingsPage = lazy(() =>
  import("./pages/ReceiverSettingsPage").then((m) => ({ default: m.ReceiverSettingsPage })),
);
const NotificationSettingsPage = lazy(() =>
  import("./pages/NotificationSettingsPage").then((m) => ({ default: m.NotificationSettingsPage })),
);
const ReportSettingsPage = lazy(() =>
  import("./pages/ReportSettingsPage").then((m) => ({ default: m.ReportSettingsPage })),
);
const TrustedServersPage = lazy(() =>
  import("./pages/TrustedServersPage").then((m) => ({ default: m.TrustedServersPage })),
);
const JobSettingsPage = lazy(() =>
  import("./pages/JobSettingsPage").then((m) => ({ default: m.JobSettingsPage })),
);
const ServerSettingsPage = lazy(() =>
  import("./pages/ServerSettingsPage").then((m) => ({ default: m.ServerSettingsPage })),
);
const CommandSettingsPage = lazy(() =>
  import("./pages/CommandSettingsPage").then((m) => ({ default: m.CommandSettingsPage })),
);
const GPGKeysPage = lazy(() =>
  import("./pages/GPGKeysPage").then((m) => ({ default: m.GPGKeysPage })),
);

// AuthGate holds the client-side redirect rules: signed-out access to
// anything but /login bounces to /login?next=..., and being signed in on
// /login bounces to /. The SSO callback route is exempt from all of this
// (see SsoCallbackPage).
function AuthGate() {
  const { ready, me } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();

  useEffect(() => {
    if (!ready) return;

    const path = location.pathname;

    // The SSO callback page finishes the provider's login and navigates on its
    // own; redirecting away first would lose the authorization response.
    if (path === SSO_CALLBACK_PATH) return;

    if (!me) {
      if (path !== "/login") {
        navigate(`/login?next=${encodeURIComponent(path + location.search)}`, { replace: true });
      }
      return;
    }

    if (path === "/login") {
      navigate("/", { replace: true });
    }
  }, [ready, me, location.pathname, location.search, navigate]);

  if (!ready) return null;

  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path={SSO_CALLBACK_PATH} element={<SsoCallbackPage />} />
      {/* Until AuthGate's effect has navigated a signed-out user to /login,
          render nothing rather than a dashboard whose every call would 401. */}
      <Route element={me ? <Layout /> : null}>
        <Route index element={<DashboardPage />} />
        <Route path="job-settings" element={<JobSettingsPage />} />
        <Route path="server-settings" element={<ServerSettingsPage />} />
        <Route path="command-settings" element={<CommandSettingsPage />} />
        <Route path="gpg-keys" element={<GPGKeysPage />} />
        <Route path="receiver-settings" element={<ReceiverSettingsPage />} />
        <Route path="trusted-servers" element={<TrustedServersPage />} />
        <Route path="notification-settings" element={<NotificationSettingsPage />} />
        <Route path="report-settings" element={<ReportSettingsPage />} />
        <Route path="identity" element={<IdentityPage />} />
        <Route path="tokens" element={<TokensPage />} />
        <Route path="logs" element={<LiveLogsPage />} />
        <Route path="logs/job-runs" element={<JobRunsPage />} />
        <Route path="logs/target-runs" element={<TargetRunsPage />} />
        <Route path="logs/login" element={<LoginLogPage />} />
        <Route path="logs/downloads" element={<DownloadLogPage />} />
        <Route path="logs/receivers" element={<ReceiverLogPage />} />
        <Route path="logs/audit" element={<AuditLogPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

export function App() {
  return (
    <AuthProvider>
      <AuthGate />
    </AuthProvider>
  );
}
