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
  // kind is the job's type; source/watching are an on-change job's source
  // path and whether it's watched for changes between scans. Absent from
  // older instances, meaning "backup".
  kind?: JobKind;
  source?: string;
  watching?: boolean;
  interval?: string;
  state: RunState;
  last_start: string;
  last_end: string;
  next_run: string;
  duration?: string;
  size?: string;
  error?: string;
  targets: TargetSnapshot[];
  // files mirrors JobSnapshot.Files: each of the backup's files as of the
  // last finished run, one per command.
  files?: FileSnapshot[];
}

// FileSnapshot mirrors backup.FileSnapshot: whether one file of a job's
// last run was produced (not whether each target received it).
export interface FileSnapshot {
  name: string;
  command: string;
  state: RunState;
  size?: string;
  error?: string;
}

export interface ReceiverSnapshot {
  id: string;
  // kind is absent from older instances, meaning "backup".
  kind?: ReceiverKind;
  path: string;
  retention?: string;
  keep_versions?: number;
  state: RunState;
  last_key?: string;
  last_seen: string;
  error?: string;
  stale_after?: string;
  stale?: boolean;
}

// PeerSnapshot mirrors status.PeerSnapshot (GET /api/peers): the status
// another instance last pushed to this one. name is its trusted server's
// name here; offline is set once it stopped pushing, jobs and receivers
// then being the last it pushed.
export interface PeerSnapshot {
  id: string;
  name: string;
  instance_name?: string;
  last_seen: string;
  interval: string;
  offline?: boolean;
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
}

// One message on the /api/live WebSocket (liveStatusJSON in
// internal/backup/webui/live.go): the full current job, receiver, and
// pushed instance state. peers is absent from older instances.
export interface LiveStatusMessage {
  type: "status";
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
  peers?: PeerSnapshot[];
}

// ReceiverFile mirrors backup.ReceiverFile. file is set for one file of a
// multi-file backup, key then being the backup's key; a flat object has
// only key. Its download path is key/file. On a directory receiver each
// entry is one version of a file: path is the file's path as sent, version
// and sha256 (of the plaintext) identify the version, and key is the
// version's own download path.
export interface ReceiverFile {
  key: string;
  file?: string;
  size: number;
  mod_time: string;
  expires_at: string;
  path?: string;
  version?: number;
  sha256?: string;
}

// JobKind mirrors config.JobKind: a job's type.
export type JobKind = "backup" | "on-change";

// ReceiverKind mirrors config.ReceiverKind: a receiver's type.
export type ReceiverKind = "backup" | "directory";

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
  type: ReceiverKind;
  // keep_versions is a directory receiver's keep-versions: 0 sets no
  // version rule.
  keep_versions: number;
  allowed_servers: string[];
  path: string;
  retention: string;
  stale_after: string;
  stale_notifications: string[];
  download_notifications: string[];
  // max_object_size and quota are sizes as entered (e.g. "10GiB"); ""
  // means the 50GiB default and no quota.
  max_object_size: string;
  quota: string;
  // immutable (backup receivers only) refuses overwriting a stored object.
  immutable: boolean;
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
  history_days: number;
  default_history_days: number;
  max_history_days: number;
}

// LogRetention mirrors settings.LogRetention: limits for the job run and
// target run logs, 0 meaning no limit of that kind.
export interface LogRetention {
  job_run_days: number;
  job_run_max_rows: number;
  target_run_days: number;
  target_run_max_rows: number;
}

// LogRetentionConfigJSON mirrors logRetentionConfigJSON (GET
// /api/log-retention-config).
export interface LogRetentionConfigJSON extends LogRetention {
  updated_at: string | null;
  updated_by: string;
  defaults: LogRetention;
  limit_days: number;
  limit_rows: number;
}

// StatusPush mirrors settings.StatusPush: whether this instance pushes its
// status to the central instance server (a remote server) names, at the
// latest every interval_seconds (0 meaning the default).
export interface StatusPush {
  enabled: boolean;
  server: string;
  interval_seconds: number;
}

// StatusPushConfigJSON mirrors statusPushConfigJSON (GET
// /api/status-push-config).
export interface StatusPushConfigJSON extends StatusPush {
  updated_at: string | null;
  updated_by: string;
  default_interval_seconds: number;
  min_interval_seconds: number;
  max_interval_seconds: number;
}

// ReportSummaryJSON mirrors reportSummaryJSON (GET /api/reports): one kept
// report without its content.
export interface ReportSummaryJSON {
  id: number;
  generated_at: string;
  period_start: string;
  period_end: string;
  trigger: "scheduled" | "manual";
  triggered_by: string;
  notifications: string[];
  receivers: number;
  receiver_errors: number;
  stale: number;
  jobs: number;
  job_errors: number;
}

// ReportJSON mirrors reportJSON (GET /api/reports/{id}, POST /api/reports).
export interface ReportJSON extends ReportSummaryJSON {
  content: {
    receivers: { id: string; files_received: number; bytes_received: number; errors: number }[];
    stale: { id: string; stale_after_seconds: number; last_seen: string | null }[];
    receiver_errors: {
      at: string;
      receiver_id: string;
      kind: string;
      key: string;
      error: string;
    }[];
    jobs: { id: string; runs_completed: number; bytes_written: number; errors: number }[];
    job_errors: { at: string; job_name: string; error: string }[];
  };
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

// JobFileJSON mirrors config.FileJobFile.
export interface JobFileJSON {
  command: string;
  name?: string;
}

// JobDefinitionJSON mirrors config.FileJob's JSON form: a job's editable
// fields, as sent to POST/PUT /api/job-configs.
// JobSourceJSON mirrors config.FileJobSource: an on-change job's directory
// tree.
export interface JobSourceJSON {
  path: string;
  exclude: string[];
  watch: boolean;
  debounce: string;
}

export interface JobDefinitionJSON {
  name: string;
  // type is "backup" (or "", the default) or "on-change". An on-change job
  // has source instead of files and key.
  type: JobKind | "";
  source?: JobSourceJSON | null;
  // files are the backup's files: each a commands entry (by id) whose
  // encrypted output is stored as name (default "<command>.gpg", ".asc"
  // with armor) under the backup's key.
  files: JobFileJSON[];
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
  // TLS verification of a remote https endpoint. tls_insecure_skip_verify
  // accepts any certificate (and excludes the other two);
  // tls_skip_hostname_verify still verifies the chain but not that it names
  // the endpoint's host; tls_ca is PEM certificates that replace the system
  // roots. All unset is the default verification.
  tls_insecure_skip_verify: boolean;
  tls_skip_hostname_verify: boolean;
  tls_ca: string;
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

// CommandType mirrors config.CommandKind; "" (from before commands had a
// type) means "shell".
export type CommandType = "" | "shell" | "tar";

// CommandDefinitionJSON mirrors config.FileCommand's JSON form: a job's
// backup source, or (shell only) a job target's on-error/on-recover hook.
// A shell command runs cmd; container, when set, runs it inside that running
// container through the Docker socket, and container_user overrides its
// default user. A tar command archives path on this machine instead, leaving
// out entries matching an exclude glob, gzip-compressed when compress is
// "gzip".
export interface CommandDefinitionJSON {
  id: string;
  type: CommandType;
  cmd: string;
  timeout: string;
  container: string;
  container_user: string;
  path: string;
  exclude: string[] | null;
  compress: string;
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

// DockerContainerJSON mirrors dockerexec.Container: one running container on
// the Docker daemon commands with a container run in.
export interface DockerContainerJSON {
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
}

// DockerContainerListJSON mirrors dockerContainerListJSON (GET
// /api/docker-containers).
export interface DockerContainerListJSON {
  containers: DockerContainerJSON[];
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
