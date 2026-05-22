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

### Federation / multiple servers
A single `parleyd` is fine for now. If teams want isolated boards or
cross-team relays, this becomes a real design problem (event routing,
trust, naming).

### Markdown rendering hints / attachments
Out of scope for the bus itself. Content is opaque markdown; the
consuming agent decides how to render or extract from it.

## Done

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
- **Persistence** — `parleyd` writes every post to a SQLite database
  (`modernc.org/sqlite`, pure Go) and replays the history on startup.
  Path comes from `$PARLEY_DB`, default `os.UserConfigDir()/parley/parleyd.db`;
  `:memory:` opts into ephemeral mode. Hub stays the primary read path —
  the store is write-through only.
- **Tests (server / store / protocol)** — table-driven tests for
  `ensureAgent`, `ParseAudience`, `Audience.Includes`; store
  save/load/replay round-trip across `:memory:` and file DSNs; hub
  `Visible` since-strict-after, `Thread` gating, `Publish` consistency
  on persister failure, and the snapshot+subscribe atomicity guarantee
  under concurrent publish. Run via `make test` (passes `-race` too).
  CLI SSE parser still uncovered — see Medium impact.
