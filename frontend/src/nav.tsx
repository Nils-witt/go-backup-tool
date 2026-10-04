import type { ReactNode } from "react";
import DashboardIcon from "@mui/icons-material/SpaceDashboard";
import StorageIcon from "@mui/icons-material/Storage";
import TerminalIcon from "@mui/icons-material/Terminal";
import LoginIcon from "@mui/icons-material/Login";
import CloudDownloadIcon from "@mui/icons-material/CloudDownload";
import HistoryIcon from "@mui/icons-material/History";
import PlaylistAddCheckIcon from "@mui/icons-material/PlaylistAddCheck";
import CloudUploadIcon from "@mui/icons-material/CloudUpload";
import VpnKeyIcon from "@mui/icons-material/VpnKey";
import KeyIcon from "@mui/icons-material/Key";
import type { AuthState } from "./auth/useAuth";

export interface NavItem {
  to: string;
  label: string;
  icon: ReactNode;
  // visible defaults to "always shown once the account has loaded" when
  // omitted — most pages have no permission gate of their own beyond "you're
  // logged in at all".
  visible?: (auth: AuthState) => boolean;
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
      {
        to: "/tokens",
        label: "API tokens",
        icon: <KeyIcon />,
        visible: (s) => s.canManageTokens,
      },
    ],
  },
  {
    label: "Logs",
    items: [
      { to: "/logs", label: "Live logs", icon: <TerminalIcon /> },
      {
        to: "/logs/job-runs",
        label: "Job runs",
        icon: <HistoryIcon />,
        visible: (s) => s.canViewJobRunLog,
      },
      {
        to: "/logs/target-runs",
        label: "Target runs",
        icon: <PlaylistAddCheckIcon />,
        visible: (s) => s.canViewTargetRunLog,
      },
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
      {
        to: "/logs/receivers",
        label: "Receiver log",
        icon: <CloudUploadIcon />,
        visible: (s) => s.canViewReceiverLog,
      },
    ],
  },
];
