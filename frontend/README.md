# go-backup-tool dashboard

The web UI dashboard: a Vite + React + TypeScript SPA, built and embedded
into the `go-backup-tool` binary via `go:embed` (see
`internal/backup/webui/webui.go`). It's a plain client of that package's
JSON `/api/...` endpoints — nothing here changes the API surface.

## Build prerequisite

`internal/backup/webui` embeds this project's build output
(`internal/backup/webui/dist/`, gitignored, not committed). That means
`go build ./...` / `go test ./...` / `go vet ./...` at the repo root will
**fail to compile** until it's been built at least once:

```sh
cd frontend
npm ci
npm run build
```

## Local development

Two terminals: the Go backend serving real data, and Vite's dev server for
fast HMR against it.

```sh
# terminal 1, from the repo root
go run ./cmd/go-backup-tool -config <your-config.yaml> -listen 127.0.0.1:8080

# terminal 2
cd frontend
npm run dev
```

`vite.config.ts` proxies `/api` and `/login` to `127.0.0.1:8080`, so the
dev server (typically `http://localhost:5173`) can call the real backend
with no CORS handling needed.

One exception: OIDC's `/login/oidc/callback` uses an absolute,
provider-configured redirect URI, so that leg of SSO login won't loop back
through the Vite dev server. Test OIDC against the full built binary
(`npm run build`, then run the Go binary directly and open its own
`-listen` address) instead.

The login page itself (`internal/backup/webui/login.html`) and the OIDC
callback bridge page (`oidc_complete.html`) are intentionally **not** part
of this SPA — they're small, server-rendered pages handling
auth/session token handoff, kept outside `frontend/` on purpose.
