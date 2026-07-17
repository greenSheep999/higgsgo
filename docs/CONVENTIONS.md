# higgsgo Coding & Documentation Conventions

**All code artifacts must be in English. No Chinese anywhere.**

Scope: `higgsgo/` and future sibling repos (`higgs-cpa-plugin/`, `higgsgo-webui/`).

---

## 1. Language rule

| Artifact | Language |
|---|---|
| README, docs, design notes | **English** |
| Code comments (inline, block, doc) | **English** |
| Commit messages | **English** |
| PR titles & descriptions | **English** |
| Log messages (structured & free-form) | **English** |
| Error messages returned to API clients | **English** |
| Config file comments | **English** |
| Migration file names & up/down SQL comments | **English** |
| CLI help text | **English** |
| Metrics label values | **English** (lowercase snake_case) |
| Panel/dashboard labels (if we ship WebUI) | **English** (i18n later) |

Chinese is fine in:
- Slack / issue tracker human discussion
- Ephemeral debugging notes not committed
- User-facing content of the WebUI **only if** we later add an i18n layer with `en` as the source of truth

---

## 2. Naming

### Go

- Packages: `lowercase`, one word if possible (`pool`, `proxy`, `register`)
- Types: `CamelCase` exported, `camelCase` unexported
- Interfaces: no `-er` suffix mandate but prefer it when it fits (`Notifier`, `Fetcher`); domain interfaces without natural verb use `Provider` or `Store` (`ProxyProvider`, `AccountStore`)
- Files: `snake_case.go` (Go standard: `account_pool.go`)
- Test files: `*_test.go`
- Internal-only interfaces stay in `internal/ports/`, implementations in `internal/adapters/`

### Config keys

- TOML / YAML keys: `snake_case`
- Environment variables: `HIGGSGO_UPPER_SNAKE`
- Feature flags: `FEATURE_<VERB>_<NOUN>` (`FEATURE_ENABLE_CPA_MODE`)

### DB

- Tables: `snake_case` plural (`accounts`, `usage_events`)
- Columns: `snake_case`
- Indices: `idx_<table>_<cols>`
- Migrations: `NNN_<verb>_<noun>.sql` (`003_create_usage_events.sql`)

### API

- Public endpoints (`/v1/*`): OpenAI-compatible naming (`/v1/videos/generations`, `/v1/models`)
- Admin endpoints (`/admin/*`): REST-ish (`/admin/accounts/{id}`)
- Internal endpoints for CPA plugin (`/internal/*`): also REST-ish
- Query params: `snake_case`
- JSON keys in request/response: `snake_case` (match OpenAI's style; NB OpenAI actually uses snake_case despite JS convention)

---

## 3. Doc structure standard

Every new Markdown doc under `docs/` starts with:

```markdown
# <Title>

> One-sentence purpose statement.
>
> Status: draft | review | approved | superseded
> Owner: <name or team>
> Last reviewed: YYYY-MM-DD

---

## 1. Context / Motivation

## 2. ...

## N. Open Questions

## Appendix
```

Design docs referencing external evidence (e.g., empirical test results) link to the raw data file under `data/` or `server/data/`.

---

## 4. Code comments

- **What the code does** goes in doc comments (before the func/type).
- **Why non-obvious decisions** goes in inline comments.
- Skip trivial comments (`// increment i by 1`).
- Reference issues / PRs / commits in comments when relevant: `// See #42.`
- Reference higgsfield-specific quirks in comments so readers don't repeat mistakes:

  ```go
  // JWT expires after 60s. Refresh at 40s cadence to leave 20s buffer.
  // See docs/HIGGSGO-ARCHITECTURE.md §9.
  ```

- TODO / FIXME format: `// TODO(<name>): <what>` — always attributable.

---

## 5. Error messages

- **Never** leak internal state to API clients.
- User-facing: short imperative English (`"model 'foo' not found"`, `"quota exceeded for this API key"`).
- Internal logs: full context (account_id, jst, retry count, etc.) via structured `slog` fields.
- Wrap errors with `fmt.Errorf("phase X: %w", err)` for traceability.

---

## 6. Log messages

- Level:
  - `debug`: request/response bodies, JWT contents (dev only)
  - `info`: request completed, account picked, job status transitions
  - `warn`: fallback taken, retry, degraded path
  - `error`: unexpected upstream response, DB error
  - **no** `fatal` in library code; only in `main.go` startup
- Format: structured JSON via `slog`
- Standard fields: `request_id`, `api_key_id`, `account_id`, `model_alias`, `jst`, `elapsed_ms`
- Messages: lowercase, no trailing period (`"account picked from pool"`, not `"Account picked from pool."`)

---

## 7. Empirical grounding

If a decision is based on empirical testing, link the evidence:

```go
// Plus and Pro accounts both return 403 unlimited_generation_not_allowed
// on POST /jobs/v2/*_unlimited. Verified 2026-07-17.
// See server/data/d-class-unlimited-retry.json for raw responses.
```

---

## 8. Open questions

- Do we adopt English identifier names for account emails or preserve as-is? (Preserve as-is — they're data, not code.)
- Chinese log messages from Node subprocesses (registrar CloakBrowser): capture and translate at bridge boundary, or pass through? (Translate at bridge boundary to keep logs uniform.)
