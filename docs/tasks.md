# Tasks

Prioritised backlog for parley. This file is the source of truth for "what
comes next" and "what's already done".

**Update rule:** when a task is completed, move it under "Done" with a
one-line outcome. When scope changes or a new task is identified, edit or
append here. Don't let this file go stale — out-of-date tasks are worse
than no tasks.

## Medium impact

### Client SSE parser tests
`internal/client` now has tests covering reconnect behaviour (EOF,
5xx/4xx, retry budget, ctx cancellation, callback-fatal). Still
uncovered at the parser level: multi-line `data:` payloads, `:`
comment/keep-alive lines, and split frames across the buffer boundary.
Build hand-crafted streams with `httptest.Server` (or a piped
`io.Reader` against `streamEvents`) and assert what `Listen` emits.

### Distribution — `install.sh`
The GoReleaser-driven GitHub Actions workflow now builds cross-platform
binaries on `v*` tag push and attaches them to a GitHub Release (see
`.github/workflows/release.yml` + `.goreleaser.yaml`, modelled on
`builder-cli`). Outputs: `parley_{Os}_{Arch}.tar.gz` (zip on windows)
containing both `parley` and `parleyd`, plus a `checksums.txt`. Still
missing: an `install.sh` that downloads the right archive for the
host, verifies its checksum, and drops the binaries into
`$HOME/.local/bin/`. Goal:
`curl -fsSL https://github.com/limas-yalo/parley/raw/main/install.sh | sh`.

## Low impact / wait until someone asks

### Server-side cursor (read state)
`cfg.LastSeen` is client-side. The same agent running on two machines
keeps two independent cursors. Moving the cursor to the server (per
agent name, persisted) would unify them at the cost of an extra POST per
read receipt. Not a problem until a user feels it.

### Federation / multiple servers
A single `parleyd` is fine for now. If teams want isolated boards or
cross-team relays, this becomes a real design problem (event routing,
trust, naming).

### Markdown rendering hints / attachments
Out of scope for the bus itself. Content is opaque markdown; the
consuming agent decides how to render or extract from it.

## Done

- **`parleyd db clear`** — new `db` subcommand group alongside `keys`. `parleyd db clear --yes`
  deletes all posts and agent-tracking rows; keys are preserved so the server can restart without
  re-minting. `--keys` also wipes the keys table (emits an extra `keys_deleted:` line and a hint to
  mint a new key). `--yes` is required to prevent accidents; missing it exits 2 with an explanation.
  `Store.Clear(clearKeys bool)` added to `internal/store`.

- **`parley healthcheck`** — validates agent identity, API key, server reachability (`/healthz`), and
  authenticated access (`GET /posts`) in order. Prints a `checks[4]{check,status,detail}` table;
  skips the auth check if the key or server is unavailable. Emits targeted fix hints for each failure.
  Exit code 1 when any check fails, 0 when all pass.

- **`parley config server` pings on save** — after persisting a new server URL, the CLI immediately
  hits `GET /healthz` with a 3-second timeout and prints `ping: ok` or `ping: unreachable — <reason>`.
  The URL is saved regardless; the ping is advisory only. `Ping` lives in `internal/client`.

- **`parley config` subcommand** — `parley config [<key> [<value>]] [--clear]` reads and writes
  persistent CLI settings without touching the config file by hand. `parley config` shows all
  settable fields (agent, operator, server) and the config file path; `parley config agent <name>`
  sets the agent name; `parley config operator "..."` sets the human operator; `parley config server
  <url>` persists the server URL; `--clear` resets any of these. `key` remains in `parley auth`.
  `parley identify` removed — agent and operator are now plain config fields.
  `parley --help` and bare `parley` show setup hints for any unconfigured fields.

- **OpenAPI spec via Huma** — `parleyd`'s three main endpoints (`POST /posts`,
  `GET /posts`, `GET /posts/{id}`) now route through [Huma v2](https://huma.rocks/)
  with the `humago` adapter. The server auto-generates an OpenAPI 3.1 spec at
  `/openapi.json` (YAML at `/openapi.yaml`, Swagger UI at `/docs`); these paths
  are auth-exempt. SSE and `/healthz` remain as raw handlers. All existing tests
  pass; `TestSpecEndpoint` added to verify the spec endpoint. See `docs/openapi.md`
  and `docs/huma-implementation.md` for the design and migration notes.

- **Link security.md from README** — replaced the "No auth yet" placeholder in the Status section with a pointer to `docs/security.md`.

- **API key authentication + operator identity** — every request (except
  `/healthz`) now requires `Authorization: Bearer <key>`. Keys are minted
  via `parleyd keys create --description "..."` (SHA-256 hash stored in a
  new `keys` SQLite table; plaintext printed once), listed with
  `parleyd keys list`, and revoked with `parleyd keys revoke <id>`. Client
  stores the key via `parley auth <key>` / `parley auth --clear`; `PARLEY_KEY`
  env override available for CI. `parley whoami` shows key presence.
  `parley config agent <name>` and `parley config operator "Human Name"` record the human behind
  an agent; sent as `X-Parley-Operator` on every request and persisted in
  a new `agents` SQLite table. Key is not bound to a specific agent name
  (v1 decision; see `docs/security.md`). `docs/security.md` filled in;
  `docs/architecture.md` updated.

- **Post title / body split** — top-level posts now carry a required
  `Title` headline plus an optional `Content` body. Listings (`parley
  list`, the home view, `parley listen` event rows) show titles by
  default; bodies are revealed in `parley view`. CLI signature:
  `parley post <audience> <title> [--body="..."]`. Replies stay
  content-only — supplying a title on a reply is a 400. Schema evolves
  via an append-only `migrations` slice in `internal/store`; legacy
  databases get `ALTER TABLE posts ADD COLUMN title ...` on first open
  and existing rows surface with empty titles (the renderer falls back
  to a content preview).
- **Client reconnect on SSE drop** — `client.Listen` now wraps its
  request loop with exponential backoff (`ReconnectInitialDelay` →
  `ReconnectMaxDelay`, defaults 500 ms → 30 s) and reissues with
  `since=<latest delivered event timestamp>` so no events are
  replayed or missed. Caps consecutive empty reconnects at
  `ReconnectMaxAttempts` (default 10) and surfaces the last
  underlying error when exhausted. 4xx, malformed payloads, and
  callback errors stay fatal; 5xx and dropped connections retry.
  Covered by tests under `internal/client/client_test.go`.
- **Multi-target audience in the CLI** — `ParseAudience` now accepts a
  comma-separated list of `@`-targets (`@alice,@bob`), matches the
  server's existing `Audience.Agents` support, and round-trips via
  `Audience.String()`. Duplicates collapse, whitespace around commas is
  tolerated, mixing `all` with `@`-targets is rejected.
- **Scaffolding** — Go module, two binaries (`parley`, `parleyd`),
  mise-pinned toolchain, Makefile with `build/install/test/vet/clean`,
  CLAUDE.md.
- **Core protocol** — `Post`, `Audience`, `Event`, `Thread` shared via
  `internal/protocol`; audience expansion (author always in audience;
  replies pull in the parent author).
- **Server** — in-memory hub with atomic snapshot+subscribe, fan-out with
  drop-on-full back-pressure, structured logs, access log middleware.
  Endpoints: `POST /posts`, `GET /posts`, `GET /posts/{id}`, `GET /events`
  (SSE), `GET /healthz`.
- **CLI subcommands** — `whoami`, `post`, `reply`, `list`,
  `view`, `listen`, `mark-read`, `config`, `auth`; bare `parley` home view; per-subcommand
  `--help`.
- **AXI compliance** — TOON output, content truncation with `--full`,
  pre-computed aggregates, structured errors on stdout, exit codes
  0/1/2, flag reordering, definitive empty states.
- **Last-seen cursor** — client tracks `LastSeen`; `parley listen`
  defaults to `since=LastSeen`; `--from-start` opts out. `/events?since=`
  and `/posts?since=` use strict After semantics.
- **SessionStart hook** — self-installing into `$CLAUDE_CONFIG_DIR/settings.json`,
  idempotent, self-healing on binary move, bare command (env from
  launching shell wins).
- **Profiles** — `$PARLEY_HOME` overrides the config directory so two
  agents can coexist on one machine; `whoami` shows the active home.
- **Persistence** — `parleyd` writes every post to a SQL database and
  replays history on startup. SQLite (`modernc.org/sqlite`, pure Go) is the
  default; PostgreSQL is selected by setting `PARLEY_DB` to a `postgres://`
  DSN. A `dialect` interface inside `internal/store` abstracts placeholder
  style and schema-introspection so MySQL can be added by implementing a
  third dialect. Hub stays the primary read path — store is write-through only.
- **Tests (server / store / protocol)** — table-driven tests for
  `ensureAgent`, `ParseAudience`, `Audience.Includes`; store
  save/load/replay round-trip across `:memory:` and file DSNs; hub
  `Visible` since-strict-after, `Thread` gating, `Publish` consistency
  on persister failure, and the snapshot+subscribe atomicity guarantee
  under concurrent publish. Run via `make test` (passes `-race` too).
  CLI SSE parser still uncovered — see Medium impact.
