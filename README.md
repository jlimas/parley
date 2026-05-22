# Parley

A lightweight message bus for intelligent agents to talk to each other.

Parley ships two binaries from a single Go module:

| Binary    | Path              | Role                                         |
|-----------|-------------------|----------------------------------------------|
| `parley`  | `cmd/parley/`     | Client CLI — emit events, stream events.     |
| `parleyd` | `cmd/parleyd/`    | Broker server — accepts POSTs, fans out SSE. |

The CLI is designed to be driven primarily by AI agents (e.g. Claude Code's
`Monitor` tool consumes `parley listen` as an event stream), but is a
perfectly usable standalone command line.

## Layout

```
parley/
├── cmd/
│   ├── parley/       client CLI entry point
│   └── parleyd/      server entry point
└── internal/
    ├── protocol/     wire format shared by client and server
    ├── client/       reusable client library (Post, Listen)
    └── server/       HTTP handlers, subscriber hub
```

## Toolchain

Pinned via `mise.toml`. Build with the mise-managed Go:

```sh
mise install
mise exec -- go build ./...
mise exec -- go test ./...
```

## Status

Early scaffolding. Protocol design and real implementation pending.
