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

// One message on the /api/live WebSocket (liveStatusJSON in
// internal/backup/webui/live.go): the full current job and receiver state.
export interface LiveStatusMessage {
  type: "status";
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
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

export interface ReceiverEventJSON {
  at: string;
  receiver_id: string;
  kind: string;
  key: string;
  size: number;
  success: boolean;
  error: string;
}

// MeJSON is GET /api/me's (and POST /api/sso/login's) response: the
// signed-in user's name and the permissions their SSO token grants.
export interface MeJSON {
  username: string;
  permissions: string[];
  admin: boolean;
}

// SSOStatusJSON is the public GET /api/sso/status response: what the SPA
// needs to run the OIDC login itself as a public client.
export interface SSOStatusJSON {
  enabled: boolean;
  buttonLabel: string;
  issuerUrl?: string;
  clientId?: string;
  scopes?: string;
}

export interface APITokenJSON {
  id: string;
  name: string;
  permissions: string[];
  created_by: string;
  created_at: string;
  expires_at: string;
  revoked_at?: string;
  revoked_by?: string;
}

/** POST /api/tokens' response: the new token plus its signed JWT, shown only once. */
export interface CreatedAPITokenJSON extends APITokenJSON {
  token: string;
}

export interface MetaJSON {
  version: string;
  commit: string;
  instanceName?: string;
}

export interface DownloadTicketJSON {
  ticket: string;
}
