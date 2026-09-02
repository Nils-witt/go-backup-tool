// Wire shapes for internal/backup/webui's JSON API, hand-ported from the
// corresponding *JSON structs in internal/backup/webui/webui.go (and
// JobSnapshot/TargetSnapshot/ReceiverSnapshot/ReceiverFile in
// internal/backup/{status,receiver}.go). Keep field names/casing in sync
// with those Go structs when the API changes.

export type RunState = "idle" | "running" | "ok" | "incomplete" | "failed";

export interface TargetSnapshot {
  server: string;
  bucket: string;
  kind: string;
  state: RunState;
  error?: string;
}

export interface JobSnapshot {
  name: string;
  interval?: string;
  state: RunState;
  last_start: string;
  last_end: string;
  next_run: string;
  duration?: string;
  size?: string;
  error?: string;
  targets: TargetSnapshot[];
}

export interface ReceiverSnapshot {
  id: string;
  path: string;
  retention?: string;
  state: RunState;
  last_key?: string;
  last_seen: string;
  error?: string;
  stale_after?: string;
  stale?: boolean;
}

export interface ReceiverFile {
  key: string;
  size: number;
  mod_time: string;
  expires_at: string;
}

export interface IdentityJSON {
  uuid: string;
  public_key: string;
}

export interface LoginEventJSON {
  at: string;
  username: string;
  method: string;
  success: boolean;
  remote_addr: string;
  detail: string;
}

export interface DownloadEventJSON {
  at: string;
  username: string;
  receiver_id: string;
  key: string;
  success: boolean;
  remote_addr: string;
  detail: string;
}

export interface JobRunEventJSON {
  job_name: string;
  start: string;
  end: string;
  success: boolean;
  size: number;
  error: string;
}

export interface TargetRunEventJSON {
  at: string;
  job_name: string;
  target: string;
  success: boolean;
  state: RunState;
  error: string;
}

export interface SessionInfoJSON {
  username: string;
  permissions: string[];
  admin: boolean;
  oidc_enabled: boolean;
}

export interface MetaJSON {
  version: string;
  commit: string;
  auth_enabled: boolean;
  oidc_enabled: boolean;
}

export interface WebUIUserJSON {
  username: string;
  oidc_username: string;
  permissions: string[];
  created_at: string;
}

export interface WebUIUserRequestJSON {
  username: string;
  password: string;
  oidc_username: string;
  permissions: string[];
}

export interface WebUIUserUpdateRequestJSON {
  oidc_username: string;
  permissions: string[];
}

export interface ApiTokenJSON {
  jti: string;
  created_at: string;
  expires_at: string;
  revoked: boolean;
  revoked_at?: string;
}

export interface ApiTokenRequestJSON {
  days: number;
}

export interface LoginResponseJSON {
  token: string;
  expires_at: string;
}

export interface DownloadTicketJSON {
  ticket: string;
}

export const PERMISSIONS = ["view", "download", "login-log", "download-log", "admin"] as const;
export type Permission = (typeof PERMISSIONS)[number];
