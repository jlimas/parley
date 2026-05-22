# OpenAPI

Parley exposes an OpenAPI 3.1 spec for `parleyd`'s HTTP API. The spec is
generated at build time from Go types — not hand-written — so it stays in
sync with the implementation automatically.

## Why OpenAPI

Two consumers benefit from a machine-readable spec:

- **parley-cloud** — the proprietary web app and management API need a
  typed HTTP client to talk to managed `parleyd` instances. Generating
  that client from the spec avoids duplicating the wire format.
- **Third-party integrations** — self-hosters can generate clients in any
  language supported by `openapi-generator` or equivalent.

## Tooling: Huma

[Huma v2](https://huma.rocks/) is a Go framework that generates an
OpenAPI 3.1 spec from typed handler functions. It wraps any standard
`net/http` router via an adapter, so the existing `server.Hub` logic is
unchanged — Huma only adds the spec endpoint and request/response
validation layer.

Adapter used: `humago` — the stdlib `net/http` adapter introduced for Go
1.22+ pattern-matching mux. No additional router dependency needed.

See `docs/huma-implementation.md` for the step-by-step migration.

## Spec endpoint

Once implemented, `parleyd` serves the spec at:

```
GET /openapi.json       machine-readable OpenAPI 3.1
GET /openapi.yaml       same, YAML form
GET /docs               Swagger UI (HTML)
```

These three endpoints are exempt from API key authentication (same
rationale as `/healthz` — discovery should not require a key).

## Security scheme

The spec declares one security scheme, `bearerAuth`, matching the live
auth behaviour:

```yaml
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: prl_<key>
```

Applied globally to all endpoints except `/healthz`, `/openapi.*`, and
`/docs`.

## Client generation

### Go (parley-cloud backend or tests)

Use `oapi-codegen` to emit a typed Go client from the spec:

```sh
go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
  --config oapi-codegen.yaml \
  http://localhost:18080/openapi.json
```

A minimal `oapi-codegen.yaml`:

```yaml
package: parleyclient
generate:
  client: true
  models: true
output: client.gen.go
```

### TypeScript (parley-cloud frontend)

Use `openapi-typescript` to generate a typed fetch client:

```sh
npx openapi-typescript http://localhost:18080/openapi.json -o src/parley.d.ts
```

Pair with `openapi-fetch` for a zero-codegen runtime:

```ts
import createClient from "openapi-fetch";
import type { paths } from "./parley.d.ts";

const client = createClient<paths>({ baseUrl: "https://parley.example.com" });
```

## Versioning

The spec version tracks the Go module version tag. No explicit version
field is embedded in the routes — breaking changes increment the module
major version, at which point `/openapi.json` reflects the new shape.
