# go-backup-tool

go-backup-tool runs a shell command, encrypts its output with GPG, and sends the result to one or more targets. A target is either a local directory or another go-backup-tool instance.

## Core functionality

- **Backup jobs**: each job runs a command (e.g. `mysqldump … | gzip`), encrypts its output for the configured GPG recipients, stages the result locally, and then uploads it to every target independently. A failed command never reaches a target, and a failing target does not affect the others.
- **Targets**
  - `local`: writes to `path/bucket/key` on the filesystem (NAS mount, external disk, …).
  - `remote`: uploads to another instance's receiver API. Each instance creates its own RSA key pair and UUID on first run and signs requests with it, so no shared secrets are needed.
- **Scheduling**: jobs run once, or repeat on an `interval`, optionally anchored to a `start-time` (UTC). The last successful run is stored in a SQLite state database, so missed runs are caught up once on startup.
- **Retention**: objects older than a configured age (`7d`, `168h`, …) are deleted automatically, per server, target, or receiver. Only objects the tool wrote itself are ever deleted.
- **Receivers**: an instance can accept backups from trusted servers. It can also alert when a sender stops delivering (`stale-after`). Receivers are managed by admins in the web dashboard (stored in the state database, applied without a restart); their paths are restricted to `webui.receivers-base-dir`.
- **Trusted servers**: admins register each sending instance once, by its server ID (UUID) and public key, both shown on that instance's Identity page, and then pick which trusted servers each receiver accepts. A request must be signed by an allowed server's key and carry that server's ID as its issuer. Replacing a server's key takes effect immediately on every receiver that allows it. Receivers set up with a single sender public key keep working, and the dashboard shows how to move them to trusted servers.
- **Notifications and hooks**: webhook and email notifications (optionally GPG-encrypted) for job failures, stale receivers, downloads, and scheduled cron reports. Notifications and the report are managed by admins in the web dashboard (stored in the state database, applied without a restart); the SMTP server stays in the config file. `on-error` / `on-recover` shell commands can run per target.
- **Web dashboard** (optional): a React SPA embedded in the binary that shows live job, target, and receiver status, run history, logs, and file downloads. Login is via OIDC SSO, with permissions mapped from the provider's groups.
- **Windows service**: on Windows, the binary can install and run itself as a service (`-service=install|start|stop|uninstall`).

## Usage

Everything is configured in a single YAML file. See [`config.example.yaml`](config.example.yaml), which documents every option.

```sh
cp config.example.yaml config.yaml
go-backup-tool -config config.yaml            # run all jobs
go-backup-tool -config config.yaml -job database  # run a single job
```

Flags: `-config`, `-job`, `-log-level` (`debug|info|warn|error`).

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
