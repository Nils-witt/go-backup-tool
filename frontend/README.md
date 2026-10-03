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

`vite.config.ts` proxies `/api` to `127.0.0.1:8080`, so the dev server
(typically `http://localhost:5173`) can call the real backend with no CORS
handling needed.

Login is SSO only, run entirely in the browser: the SPA is a public OpenID
Connect client (`src/auth/oidc.ts`, authorization code + PKCE via
`oidc-client-ts`) and sends the provider's access token as a bearer token on
every `/api/...` call (`src/api/client.ts`). The redirect URI is
`<origin>/login/sso/callback`, a client-side route
(`src/pages/SsoCallbackPage.tsx`), so SSO works against the dev server too
as long as the provider also allows `http://localhost:5173/login/sso/callback`
as a redirect URI and that origin for CORS.
