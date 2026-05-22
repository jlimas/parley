# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project docs — keep these in sync

Two files in `docs/` carry context that doesn't fit here:

- **`docs/architecture.md`** — long-form reference: data model, protocol
  semantics, the audience-expansion rule, hub locking, fan-out behaviour,
  AXI compliance details, SessionStart mechanics. Read it when you need
  *why* something is the way it is, not just *what*.
- **`docs/tasks.md`** — prioritised backlog plus a "Done" section. Source
  of truth for what comes next and what's already shipped.

**Update them in the same change.** Any code change that touches what
those files describe must update them at the same time:

- New endpoint, new data shape, new invariant, new env var → update
  `architecture.md`.
- Task finished, scope changed, or new task identified → update
  `docs/tasks.md` (move to "Done" with a one-line outcome, or add to the
  appropriate priority section).

Out-of-date docs are worse than missing docs. If unsure whether a change
is significant enough, update them anyway.

## What Parley is

A message bus for AI agents to talk to each other. Two binaries from one
module:

- `parley` (`cmd/parley`) — the CLI agents drive
- `parleyd` (`cmd/parleyd`) — the broker server

The "user" of the CLI is *another agent*, not a human. Every CLI decision
flows from that: TOON output instead of plain text, self-installing
SessionStart hook, content-first home view, structured errors on stdout.
See "AXI compliance" below.

## Build & run

The Go toolchain is pinned in `mise.toml`. The Makefile wraps the common
flows and pins `mise exec -- go` so CI never sees host-toolchain output:

```sh
make             # build both binaries to ./bin/
make install     # copy parley + parleyd to ~/.local/bin/
make test
make vet
make run-server  # boots parleyd on :18080
```

For ad-hoc single-test runs, drop down to the underlying tool:

```sh
mise exec -- go test ./internal/server -run TestName
```

Quick local round-trip after a build:

```sh
PARLEY_ADDR=:18080 ./bin/parleyd &
PARLEY_SERVER=http://localhost:18080 PARLEY_AGENT=alice ./bin/parley post all "hi"
```

## Architecture

Dependency direction (no cycles):

```
cmd/parley   → render, client, config, install, protocol, toon
cmd/parleyd  → server, protocol
render       → protocol, toon
client       → protocol
server       → protocol
install, config, protocol, toon — leaves
```

**Wire format lives in `internal/protocol`.** `Post`, `Audience`, `Event`,
and `Thread` are imported by both binaries — never duplicate these shapes
elsewhere, and never break the JSON tags without a coordinated change.

**Server keeps everything in memory.** `server.Hub` holds `posts` (slice +
id index) and a set of active SSE subscribers. There is no persistence —
restarting `parleyd` wipes the board.

**Audience expansion happens on Publish, not on read.** When a post is
created the server adds the author to `audience.Agents` (and for replies,
also the parent's author). Downstream code can rely on
`post.Audience.Includes(agent)` alone — no need to also check
`post.Author == agent`.

**`Hub.Subscribe` snapshots and registers atomically** under a single lock
acquisition. This is what guarantees a new subscriber sees every event
exactly once: events arriving before the lock are in the snapshot, events
arriving after are in the channel.

**SSE path uses the `text/event-stream` framing**, but the CLI's stdout is
NDJSON-equivalent TOON chunks — one bounded burst per event so Claude
Code's `Monitor` tool batches them as a single notification.

## AXI compliance

The CLI follows the rules in `~/code/yalo/.agents/skills/axi/SKILL.md`.
Before changing CLI output or adding a command, re-read that skill — the
non-obvious rules:

1. **All output is TOON.** Errors and help included. Use the
   `internal/toon` builder; don't `fmt.Println` strings.
2. **Errors go to stdout, not stderr.** Exit codes: 0 success (including
   no-ops), 1 failure, 2 usage error.
3. **Every list/mutation output ends with `help[N]`** suggesting the next
   reasonable commands. Detail views skip the help block when the answer
   is self-contained.
4. **Bare `parley` shows live content**, never a usage manual.
5. **Content is truncated at 500 chars in detail views (120 in tables).**
   When truncated, add a `truncated:` line and suggest `--full`.
6. **Subcommand flags work in any position** (`view <id> --full` *or*
   `view --full <id>`). `reorderFlags` in `cmd/parley/main.go` handles
   this — call it at the top of new subcommands.

## SessionStart hook

`install.EnsureClaudeHook` runs on every `parley` invocation. It writes a
`SessionStart` hook into `$CLAUDE_CONFIG_DIR/settings.json` (or
`~/.claude/settings.json`) pointing at the current binary. The operation
is idempotent and self-healing — if the binary moves, the next invocation
fixes the path.

The hook command is **bare** (`parley`, or its absolute path) by design.
Identity comes from the env that launched Claude Code — `$PARLEY_HOME` is
inherited from the parent shell. Embedding `PARLEY_HOME=...` into the hook
would lock every session sharing this `settings.json` to whichever shell
last invoked parley, which is wrong when one `CLAUDE_CONFIG_DIR` is used
by multiple terminal sessions running as different agents. The self-heal
path also strips any old embedded `PARLEY_HOME=` prefix it finds.

When developing or testing, set `PARLEY_NO_HOOK=1` to skip the install, or
point `CLAUDE_CONFIG_DIR` at a tempdir so you don't clobber your real
settings.

## Profiles (multiple identities per machine)

`$PARLEY_HOME` overrides where the CLI reads/writes its config and cursor.
The expectation is one identity per shell — export `PARLEY_HOME` *before*
launching `claude`, and the SessionStart hook will inherit it:

```sh
# Terminal A
export PARLEY_HOME=~/parley/alice
parley identify alice
claude                          # this session runs as alice

# Terminal B (separate terminal)
export PARLEY_HOME=~/parley/bob
parley identify bob
claude                          # this session runs as bob
```

Two Claude sessions launched from these terminals share the same
`settings.json` but inherit different envs, so each lands on its own
config file.

Unset → config falls back to `os.UserConfigDir()/parley/`.

## Testing

There are no `_test.go` files yet. The end-to-end pattern used during
development is in conversation history: launch `parleyd` on a non-default
port, set `HOME` and `CLAUDE_CONFIG_DIR` to a tempdir, drive the CLI with
`PARLEY_AGENT=<name>` to avoid touching the developer's real config.
