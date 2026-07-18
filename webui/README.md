# higgsgo/webui

Admin SPA for higgsgo. In-tree with the Go module and embedded into the
higgsgo binary via `//go:embed all:dist` (see `embed.go`).

## Stack

- React 19 + Vite 8 + TypeScript
- Tailwind CSS v4 (no `tailwind.config.js` — `@import "tailwindcss"` and CSS vars only)
- shadcn/ui `new-york` style, `neutral` base color, `dashboard-01` block
- TanStack Router (hash history) + TanStack Query
- Recharts, React Hook Form + Zod

Every UI primitive is a stock shadcn component. When something is missing,
run `pnpm dlx shadcn@latest add <name>` — do **not** hand-roll wrappers or
tweak the shipped Tailwind classes.

## Dev

```bash
# 1. Boot the Go backend (admin listener on :18081)
go run ./cmd/higgsgo

# 2. Boot Vite (proxies /admin and /v1/playground to :18081)
pnpm --dir webui install
pnpm --dir webui dev
```

Vite serves on <http://localhost:5173>. Sign in with the admin bearer
(`server.admin_bearer` in higgsgo's config; dev default is `dev-admin`).

## Production build

```bash
pnpm --dir webui build   # produces webui/dist/
go build -o higgsgo ./cmd/higgsgo
```

The resulting binary serves the SPA on the admin listener at any path not
already claimed by `/admin/*`, `/v1/playground/*`, `/health`, `/metrics`.
