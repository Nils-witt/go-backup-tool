# go-backup-tool

go-backup-tool runs a shell command, encrypts its output with GPG, and sends the result to one or more targets. A target is either a local directory or another go-backup-tool instance.

## Core functionality

- **Backup jobs**: each job runs one or more commands from the shared `commands:` list (its `files:`, e.g. `mysqldump … | gzip`, or a `type: tar` archive of `/srv`), one after another. Each command's output is encrypted for the configured GPG recipients, staged locally, and stored as one file under the backup's key (a folder, e.g. `db-<time>/dump-database.gpg`). Then every staged file is uploaded to every target independently. A failed command never reaches a target: the other files still go out and the run is reported incomplete. A failing target does not affect the others.
- **On-change jobs** (`type: on-change`): back up a directory tree file by file. Every changed file is encrypted on its own and uploaded to a directory receiver on another instance. A content hash per file and target is kept in the state database, so unchanged files are neither read again (same size and modification time) nor uploaded again (same content). Changes are picked up as they happen (`source.watch`, debounced) and by periodic full scans (`interval`).
- **Commands**: named, reusable shell commands, used as one of a job's backup files (`files:`) or as a target's `on-error`/`on-recover` hook. A `type: tar` command archives a file or directory on this machine instead (`path:`, optional `exclude:` globs and `compress: gzip`), written in-process, so no `tar` binary is needed; it can only be a backup file, not a hook. A command can run inside an already-running Docker container instead (`container:`, optionally `container-user:`). It is exec'd through the Docker socket (`docker-socket:`, else `DOCKER_HOST`, else `/var/run/docker.sock`), so no Docker CLI is needed; e.g. dump a database with the client tools in its own image.
- **Targets**
  - `local`: writes to `path/bucket/key` on the filesystem (NAS mount, external disk, …).
  - `remote`: uploads to another instance's receiver API. Each instance creates its own RSA key pair and UUID on first run and signs requests with it, so no shared secrets are needed. Per server, an https endpoint can trust a custom CA instead of the system roots, skip the hostname check, or ignore certificate errors altogether.
- **Config file or dashboard**: by default, jobs, the servers they upload to, and their `on-error`/`on-recover` commands are defined in the config file and shown read-only in the web dashboard. With `webui.job-editing: true`, admins manage them in the dashboard instead, stored in the state database (off by default, since they run shell commands on the host); the config file must then leave them out. Dashboard changes apply without a restart: a job picks up an edit from its next run on and is only rescheduled when its interval or start time changes.
- **GPG keys**: the dashboard lists the public keys in the keyring jobs encrypt to, and admins can add ASCII-armored public keys there when `webui.job-editing` is on.
- **Scheduling**: jobs run once, or repeat on an `interval`, optionally anchored to a `start-time` (UTC). The last successful run is stored in the state database, so missed runs are caught up once on startup.
- **State database**: SQLite by default, in a file next to the config file, or PostgreSQL (`database: {type: postgres, ...}` in the config file, as a `dsn:` or as separate host/port/user/password/dbname/sslmode fields). go-backup-tool creates the tables on startup and refuses to start if PostgreSQL can't be reached. Nothing is migrated from an existing SQLite file.
- **Retention**: objects older than a configured age (`7d`, `168h`, …) are deleted automatically, per server, target, or receiver. Only objects the tool wrote itself are ever deleted.
- **Directory receivers** (`type: directory`): store the files on-change jobs send under the path they were sent with, adding a new version only when a file's content changed (`<path>.gbtv/<version>-<sha256>.gpg`). Retention combines two rules, `keep-versions` (the newest N) and `retention` (an age): a version is deleted only once no rule keeps it, and the newest version of a file is always kept. The dashboard lists every file's versions for download.
- **Receivers**: an instance can accept backups from trusted servers, each backup a key folder with one file per command (the dashboard lists and downloads them per file). A sender with multi-file jobs needs receivers on this version or newer; older receivers reject the upload instead of mixing up the files. It can also alert when a sender stops delivering (`stale-after`). Receivers are managed by admins in the web dashboard (stored in the state database, applied without a restart); their paths are restricted to `webui.receivers-base-dir`.
- **Trusted servers**: admins register each sending instance once, by its server ID (UUID) and public key, both shown on that instance's Identity page, and then pick which trusted servers each receiver accepts. A request must be signed by an allowed server's key and carry that server's ID as its issuer. Replacing a server's key takes effect immediately on every receiver that allows it.
- **Notifications and hooks**: webhook and email notifications (optionally GPG-encrypted) for job failures, stale receivers, downloads, and scheduled cron reports. Notifications and the report are managed by admins in the web dashboard (stored in the state database, applied without a restart); the SMTP server stays in the config file. `on-error` / `on-recover` shell commands can run per target.
- **Web dashboard** (optional): a React SPA embedded in the binary that shows live job, target, and receiver status, run history, logs, and file downloads. Login is via OIDC SSO, with permissions mapped from the provider's groups and OIDC back-channel logout support. Every response carries security headers: a Content-Security-Policy that only lets the dashboard load from its own origin and connect to it and the OIDC provider, `X-Content-Type-Options`, frame and referrer protection, `Cache-Control: no-store` on the API, and `Strict-Transport-Security` on HTTPS requests (directly over TLS, or as reported by a trusted reverse proxy with `webui.trust-proxy-headers`). Each client's `/api/...` requests are rate-limited, with a stricter limit on failed authentications (`webui.rate-limit`).
- **Windows service**: on Windows, the binary can install and run itself as a service (`-service=install|start|stop|uninstall`).

## Usage

Instance-wide settings are configured in a single YAML file. See [`config.example.yaml`](config.example.yaml), which documents every option. Trusted servers, receivers, notifications and the report are managed in the web dashboard; jobs, servers and commands too, if `webui.job-editing` is on.

To keep the dashboard-managed settings in version control, or to copy them to another instance, export and import them as a settings file (format: [`settings.example.yaml`](settings.example.yaml)). Import creates or updates each entry by id, with the dashboard's validation, and never deletes anything:

```sh
go-backup-tool -config config.yaml -export-settings settings.yaml
go-backup-tool -config config.yaml -import-settings settings.yaml
```

```sh
cp config.example.yaml config.yaml
go-backup-tool -config config.yaml            # run all jobs
go-backup-tool -config config.yaml -job database  # run a single job
```

Flags: `-config`, `-job`, `-log-level` (`debug|info|warn|error`), `-export-settings`, `-import-settings`.

Upgrading to v8 from an older release: see [`CHANGELOG.md`](CHANGELOG.md) for the config sections v8.0.0 removed and how to carry them over.

### Docker

```sh
docker compose up -d
```

See [`compose.yaml`](compose.yaml). It mounts `config.yaml`, the `data/` directory, and a `keys/` directory with recipient public keys, which are imported on startup.

## Building

The dashboard is embedded with `go:embed`, so build the frontend first:

```sh
(cd frontend && npm ci && npm run build)
go build ./cmd/go-backup-tool
```

The API is described in [`openapi.yaml`](openapi.yaml).

## License

[AGPL-3.0](LICENSE)
