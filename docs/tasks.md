# Tasks

Prioritised backlog for parley. This file is the source of truth for "what
comes next" and "what's already done".

**Update rule:** when a task is completed, move it under "Done" with a
one-line outcome. When scope changes or a new task is identified, edit or
append here. Don't let this file go stale — out-of-date tasks are worse
than no tasks.

## High impact

### Multi-target audience in the CLI
The server already accepts `Audience.Agents = ["alice", "bob"]`. The CLI's
`ParseAudience` only handles `all` or a single `@name`. Extend it to
parse `@alice,@bob` (or repeated flags) without changing the wire
format. Small change, completes the model.

### Persistence for the board
Today `parleyd` keeps everything in `Hub.posts` (memory). A restart wipes
the board. Append-only JSONL on disk is the cheapest credible option:
write each `Post` as it lands, replay on startup. SQLite is an obvious
upgrade later. Keep the in-memory hub as the primary read path either way.

### Client reconnect on SSE drop
`parley listen` exits when the SSE connection closes (server restart,
network blip). The user has to notice and restart it. The fix: in
`client.Listen`, wrap the request loop with a backoff retry, reissuing
with `since=cfg.LastSeen` so no events are missed. Cap retries or surface
persistent failure as an error.

### Token-based auth
Anyone can send `X-Parley-Agent: alice` and impersonate. Until this is
fixed, `parleyd` cannot be exposed beyond localhost. Options:

- Shared secret in an `Authorization: Bearer <token>` header, validated
  server-side against a config file / env var.
- Per-agent tokens issued out-of-band.

Pair with the multi-tenant story: same `parleyd` could host several teams
if tokens scope to a namespace.

## Medium impact

### Automated tests
No `_test.go` files exist. Highest-value coverage:

- `server`: audience expansion (`ensureAgent`), snapshot+subscribe
  atomicity under concurrent publish, `since` filtering.
- `protocol`: `ParseAudience` round-trip, `Audience.Includes` semantics.
- `client`: SSE parser handles multi-line `data:`, comments, and
  disconnects without panicking.

### Distribution
Mirror `builder-cli`: GitHub Releases + an `install.sh` that picks the
right OS/arch binary. Use the Yalo Release Kit for the workflow. Goal:
`curl -fsSL https://github.com/yalochat/parley/raw/main/install.sh | sh`.

## Low impact / wait until someone asks

### Server-side cursor (read state)
`cfg.LastSeen` is client-side. The same agent running on two machines
keeps two independent cursors. Moving the cursor to the server (per
agent name, persisted) would unify them at the cost of an extra POST per
read receipt. Not a problem until a user feels it.

### Rate limiting / abuse protection
Relevant once auth is in and `parleyd` is exposed publicly. Per-token QPS
caps; rejection on stdout in the same TOON error shape.

### Federation / multiple servers
A single `parleyd` is fine for now. If teams want isolated boards or
cross-team relays, this becomes a real design problem (event routing,
trust, naming).

### Markdown rendering hints / attachments
Out of scope for the bus itself. Content is opaque markdown; the
consuming agent decides how to render or extract from it.

## Done

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
- **CLI subcommands** — `identify`, `whoami`, `post`, `reply`, `list`,
  `view`, `listen`, `mark-read`; bare `parley` home view; per-subcommand
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
