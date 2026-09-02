import type { ReactNode } from "react";
import DashboardIcon from "@mui/icons-material/SpaceDashboard";
import StorageIcon from "@mui/icons-material/Storage";
import TerminalIcon from "@mui/icons-material/Terminal";
import LoginIcon from "@mui/icons-material/Login";
import CloudDownloadIcon from "@mui/icons-material/CloudDownload";
import HistoryIcon from "@mui/icons-material/History";
import PlaylistAddCheckIcon from "@mui/icons-material/PlaylistAddCheck";
import VpnKeyIcon from "@mui/icons-material/VpnKey";
import GroupIcon from "@mui/icons-material/Group";
import AdminPanelSettingsIcon from "@mui/icons-material/AdminPanelSettings";
import type { SessionInfoState } from "./hooks/useSessionInfo";

export interface NavItem {
  to: string;
  label: string;
  icon: ReactNode;
  // visible defaults to "always shown while the session has loaded" when
  // omitted — most pages have no permission gate of their own beyond "you're
  // logged in at all".
  visible?: (session: SessionInfoState) => boolean;
}

export interface NavGroup {
  label?: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  {
    items: [
      { to: "/", label: "Dashboard", icon: <DashboardIcon /> },
      { to: "/receivers", label: "Receivers", icon: <StorageIcon /> },
      { to: "/identity", label: "Identity", icon: <VpnKeyIcon /> },
    ],
  },
  {
    label: "Logs",
    items: [
      { to: "/logs", label: "Live logs", icon: <TerminalIcon /> },
      { to: "/logs/job-runs", label: "Job runs", icon: <HistoryIcon /> },
      { to: "/logs/target-runs", label: "Target runs", icon: <PlaylistAddCheckIcon /> },
      {
        to: "/logs/login",
        label: "Login log",
        icon: <LoginIcon />,
        visible: (s) => s.canViewLoginLog,
      },
      {
        to: "/logs/downloads",
        label: "Download log",
        icon: <CloudDownloadIcon />,
        visible: (s) => s.canViewDownloadLog,
      },
    ],
  },
  {
    label: "Administration",
    items: [
      { to: "/admin/users", label: "Users", icon: <GroupIcon />, visible: (s) => !!s.info?.admin },
      {
        to: "/admin/oidc-users",
        label: "OIDC users",
        icon: <AdminPanelSettingsIcon />,
        visible: (s) => !!s.info?.admin && !!s.info?.oidc_enabled,
      },
    ],
  },
];
