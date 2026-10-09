import type { ReactNode } from "react";
import DashboardIcon from "@mui/icons-material/SpaceDashboard";
import TerminalIcon from "@mui/icons-material/Terminal";
import LoginIcon from "@mui/icons-material/Login";
import CloudDownloadIcon from "@mui/icons-material/CloudDownload";
import HistoryIcon from "@mui/icons-material/History";
import PlaylistAddCheckIcon from "@mui/icons-material/PlaylistAddCheck";
import CloudUploadIcon from "@mui/icons-material/CloudUpload";
import VpnKeyIcon from "@mui/icons-material/VpnKey";
import KeyIcon from "@mui/icons-material/Key";
import SettingsIcon from "@mui/icons-material/Settings";
import NotificationsIcon from "@mui/icons-material/Notifications";
import SummarizeIcon from "@mui/icons-material/Summarize";
import VerifiedUserIcon from "@mui/icons-material/VerifiedUser";
import ScheduleIcon from "@mui/icons-material/Schedule";
import DnsIcon from "@mui/icons-material/Dns";
import CodeIcon from "@mui/icons-material/Code";
import EnhancedEncryptionIcon from "@mui/icons-material/EnhancedEncryption";
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
    label: "Settings",
    items: [
      {
        to: "/job-settings",
        label: "Jobs",
        icon: <ScheduleIcon />,
        visible: (s) => s.canManageJobs,
      },
      {
        to: "/server-settings",
        label: "Servers",
        icon: <DnsIcon />,
        visible: (s) => s.canManageJobs,
      },
      {
        to: "/command-settings",
        label: "Commands",
        icon: <CodeIcon />,
        visible: (s) => s.canManageJobs,
      },
      {
        to: "/gpg-keys",
        label: "GPG keys",
        icon: <EnhancedEncryptionIcon />,
        visible: (s) => s.canManageJobs,
      },
      {
        to: "/receiver-settings",
        label: "Receivers",
        icon: <SettingsIcon />,
        visible: (s) => s.canManageReceivers,
      },
      {
        to: "/trusted-servers",
        label: "Trusted servers",
        icon: <VerifiedUserIcon />,
        visible: (s) => s.canManageReceivers,
      },
      {
        to: "/notification-settings",
        label: "Notifications",
        icon: <NotificationsIcon />,
        visible: (s) => s.canManageSettings,
      },
      {
        to: "/report-settings",
        label: "Report",
        icon: <SummarizeIcon />,
        visible: (s) => s.canManageSettings,
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
