// Wire shapes for internal/backup/webui's JSON API, hand-ported from the
// corresponding *JSON structs in internal/backup/webui/webui.go (and
// JobSnapshot/TargetSnapshot/ReceiverSnapshot/ReceiverFile in
// internal/backup/{status,receiver}.go). Keep field names/casing in sync
// with those Go structs when the API changes.

export type RunState = "idle" | "running" | "ok" | "incomplete" | "failed";

export interface TargetSnapshot {
  server: string;
  // server_uuid is a remote server's configured server-uuid: the
  // destination instance's own server UUID. Omitted when unset.
  server_uuid?: string;
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
  fingerprint: string;
}

export interface LoginEventJSON {
  at: string;
  username: string;
  method: string;
  success: boolean;
  remote_addr: string;
  detail: string;
}

export interface AuditEventJSON {
  at: string;
  username: string;
  action: string;
  resource: string;
  target: string;
  method: string;
  path: string;
  status: number;
  success: boolean;
  remote_addr: string;
  detail: string;
  changes: AuditChangeJSON[];
}

// AuditChangeJSON is one field an audit event's change set: old is absent
// when it was unset before (a create), new when it's unset after (a delete).
export interface AuditChangeJSON {
  field: string;
  old?: unknown;
  new?: unknown;
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

// ReceiverConfigJSON mirrors receiverConfigJSON in
// internal/backup/webui/receivers_config.go: one stored receiver as entered,
// plus error when it no longer resolves and so isn't currently active.
export interface ReceiverConfigJSON {
  id: string;
  allowed_servers: string[];
  // public_key is the deprecated single sender key, "" unless the receiver
  // predates trusted servers.
  public_key: string;
  path: string;
  retention: string;
  stale_after: string;
  stale_notifications: string[];
  download_notifications: string[];
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
  error?: string;
}

// ReceiverConfigListJSON mirrors receiverConfigListJSON (GET
// /api/receiver-configs).
export interface ReceiverConfigListJSON {
  base_dir: string;
  notifications: string[];
  trusted_servers: TrustedServerOptionJSON[];
  receivers: ReceiverConfigJSON[];
}

// TrustedServerOptionJSON mirrors trustedServerOptionJSON in
// internal/backup/webui/receivers_config.go: a trusted server a receiver may
// allow.
export interface TrustedServerOptionJSON {
  id: string;
  name: string;
}

// TrustedServerJSON mirrors trustedServerJSON in
// internal/backup/webui/trusted_servers.go: one stored trusted server, plus
// its key's fingerprint, the receivers allowing it, and error when it no
// longer resolves and so isn't currently active.
export interface TrustedServerJSON {
  id: string;
  name: string;
  public_key: string;
  fingerprint: string;
  used_by: string[];
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
  error?: string;
}

// TrustedServerListJSON mirrors trustedServerListJSON (GET
// /api/trusted-servers).
export interface TrustedServerListJSON {
  servers: TrustedServerJSON[];
}

// WebhookConfigJSON mirrors webhookConfigJSON in
// internal/backup/webui/settings_config.go. Header values are write-only:
// always null when read; send null back to keep a header's stored value.
export interface WebhookConfigJSON {
  url: string;
  method: string;
  headers: Record<string, string | null>;
  body: string;
}

export interface EmailConfigJSON {
  to: string[];
  from: string;
  subject: string;
  body: string;
  encrypt?: { recipients: string[] } | null;
}

// NotificationConfigJSON mirrors notificationConfigJSON.
export interface NotificationConfigJSON {
  id: string;
  webhook: WebhookConfigJSON | null;
  email: EmailConfigJSON | null;
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
  error?: string;
  used_by: string[];
}

// NotificationConfigListJSON mirrors notificationConfigListJSON (GET
// /api/notification-configs).
export interface NotificationConfigListJSON {
  smtp_configured: boolean;
  notifications: NotificationConfigJSON[];
}

// ReportConfigJSON mirrors reportConfigJSON (GET /api/report-config).
export interface ReportConfigJSON {
  enabled: boolean;
  schedule: string;
  notifications: string[];
  updated_at: string | null;
  updated_by: string;
  error?: string;
  default_schedule: string;
  next_run: string | null;
  notification_ids: string[];
}

// JobTargetJSON mirrors config.FileJobTarget's JSON form: one of a job's
// targets as entered.
export interface JobTargetJSON {
  server: string;
  bucket: string;
  retention: string;
  on_error: { command: string; after: number; repeat: boolean | null } | null;
  on_recover: { command: string } | null;
}

// JobDefinitionJSON mirrors config.FileJob's JSON form: a job's editable
// fields, as sent to POST/PUT /api/job-configs.
export interface JobDefinitionJSON {
  name: string;
  // command names the commands entry whose stdout is the backup.
  command: string;
  key: string;
  targets: JobTargetJSON[];
  recipients: string[];
  armor: boolean;
  gpg_bin: string;
  gpg_homedir: string;
  interval: string;
  start_time: string;
  staging_dir: string;
  failure_notifications: string[];
}

interface AuditJSON {
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
}

// JobConfigJSON mirrors jobConfigJSON in
// internal/backup/webui/jobs_config.go: one stored job, plus error when it
// doesn't resolve and so isn't active.
export interface JobConfigJSON extends JobDefinitionJSON, AuditJSON {
  error?: string;
}

// ServerOptionJSON mirrors serverOptionJSON: a server a job's targets may
// name.
export interface ServerOptionJSON {
  name: string;
  type: string;
}

// JobConfigListJSON mirrors jobConfigListJSON (GET /api/job-configs).
export interface JobConfigListJSON {
  editing: boolean;
  servers: ServerOptionJSON[];
  commands: string[];
  notifications: string[];
  jobs: JobConfigJSON[];
}

// ServerDefinitionJSON mirrors config.FileServer's JSON form.
export interface ServerDefinitionJSON {
  name: string;
  type: "local" | "remote" | string;
  endpoint: string;
  // server_uuid (remote only, optional) is the destination instance's server
  // UUID, as shown on its Identity page.
  server_uuid: string;
  path: string;
  retention: string;
}

// ServerConfigJSON mirrors serverConfigJSON: one stored server, the jobs
// using it, and error when it doesn't resolve.
export interface ServerConfigJSON extends ServerDefinitionJSON, AuditJSON {
  used_by: string[];
  error?: string;
}

// ServerConfigListJSON mirrors serverConfigListJSON (GET /api/server-configs).
export interface ServerConfigListJSON {
  editing: boolean;
  servers: ServerConfigJSON[];
}

// CommandDefinitionJSON mirrors config.FileCommand's JSON form: a job's
// backup source, or a job target's on-error/on-recover hook. container, when
// set, runs cmd inside that running container through the Docker socket;
// container_user overrides its default user.
export interface CommandDefinitionJSON {
  id: string;
  cmd: string;
  timeout: string;
  container: string;
  container_user: string;
}

// CommandConfigJSON mirrors commandConfigJSON: one stored command, the jobs
// using it, and error when it doesn't resolve.
export interface CommandConfigJSON extends CommandDefinitionJSON, AuditJSON {
  used_by: string[];
  error?: string;
}

// CommandConfigListJSON mirrors commandConfigListJSON (GET
// /api/command-configs).
export interface CommandConfigListJSON {
  editing: boolean;
  commands: CommandConfigJSON[];
}

// GPGKeyJSON mirrors gpgkeys.Key: one public key in the keyring jobs
// encrypt to.
export interface GPGKeyJSON {
  fingerprint: string;
  key_id: string;
  algorithm: string;
  length: number;
  created: string;
  expires?: string;
  user_ids: string[];
  revoked: boolean;
  expired: boolean;
  disabled: boolean;
  can_encrypt: boolean;
}

// GPGKeyListJSON mirrors gpgKeyListJSON (GET /api/gpg-keys). homedir is ""
// for gpg's default keyring.
export interface GPGKeyListJSON {
  homedir: string;
  editing: boolean;
  keys: GPGKeyJSON[];
}

// GPGKeyImportResultJSON mirrors gpgkeys.ImportResult (POST /api/gpg-keys).
export interface GPGKeyImportResultJSON {
  fingerprints: string[];
  warnings: string[];
}
