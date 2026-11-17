# Web frontends

Two SvelteKit + Tailwind v4 projects, statically exported and
embedded into the Go binary:

- `console/` → served by the management console (`/`).
- `app/` → served by the application server (`/`).

Both are client-rendered SPAs: the console polls `/metrics`
and `/api/overview` (keeper cards, plan, activity, TLS
bundles), the app polls `/api/status`. The embedded output
carries no build-time data.

## Layout

```
web/
  console/            SvelteKit project (adapter-static → build/)
  app/                SvelteKit project (adapter-static → build/)
  package.json        workspace root (private)
  pnpm-workspace.yaml members: console, app
```

`just web-build` (repo root) installs, type-checks, builds
both projects, and copies each `build/` into the Go embed
dirs (`internal/console/static`, `internal/app/static`).
Built, never committed (gitignored): run this (or any `up`
target, which depends on it) before `go build` on a fresh
tree — CI builds them the same way.

## Develop

```sh
# Terminal 1: Go servers with the last built UI
go run ./cmd/app serve

# Terminal 2: console frontend with API proxy to :9300
pnpm --dir web --filter console dev -- --port 5173

# Terminal 3: app frontend with API proxy to :8080
pnpm --dir web --filter app dev -- --port 5174
```

Vite proxies `/metrics`, `/health`, and `/api/*` at the
owning Go server, so `pnpm dev` works against a live backend.

## Notes

- TypeScript 7 (native `tsgo`) with a TS 6 compat install
  beside it: `svelte-check` needs both (`typescript@~6` for
  tooling, `@typescript/native` for the check behind
  `--tsgo`). SvelteKit 3, whose config lives in
  `vite.config.ts` — there is no `svelte.config.js`
  anymore.
- Asset URLs are absolute (`/_app/...`): the frontends
  assume root-mounted servers, like the old templates did.
  The console/app `*_BASE_PATH` subpath mode does not
  apply to the SvelteKit pages yet.
