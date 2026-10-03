import { useEffect } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { AuthProvider, useAuth } from "./auth/AuthContext";
import { SSO_CALLBACK_PATH } from "./auth/oidc";
import { Layout } from "./Layout";
import { LoginPage } from "./pages/LoginPage";
import { SsoCallbackPage } from "./pages/SsoCallbackPage";
import { DashboardPage } from "./pages/DashboardPage";
import { ReceiversPage } from "./pages/ReceiversPage";
import { LiveLogsPage } from "./pages/LiveLogsPage";
import { JobRunsPage } from "./pages/JobRunsPage";
import { TargetRunsPage } from "./pages/TargetRunsPage";
import { LoginLogPage } from "./pages/LoginLogPage";
import { DownloadLogPage } from "./pages/DownloadLogPage";
import { ReceiverLogPage } from "./pages/ReceiverLogPage";
import { IdentityPage } from "./pages/IdentityPage";

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
        <Route path="receivers" element={<ReceiversPage />} />
        <Route path="identity" element={<IdentityPage />} />
        <Route path="logs" element={<LiveLogsPage />} />
        <Route path="logs/job-runs" element={<JobRunsPage />} />
        <Route path="logs/target-runs" element={<TargetRunsPage />} />
        <Route path="logs/login" element={<LoginLogPage />} />
        <Route path="logs/downloads" element={<DownloadLogPage />} />
        <Route path="logs/receivers" element={<ReceiverLogPage />} />
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
