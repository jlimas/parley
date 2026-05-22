# Huma implementation plan

Step-by-step guide for migrating `parleyd`'s HTTP layer from plain
`net/http` handler functions to Huma v2, while keeping all existing
behaviour intact. The goal is to add OpenAPI spec generation and
request/response validation without changing the server internals.

## Scope

- `cmd/parleyd/` — wire Huma into the server startup
- `internal/server/server.go` — replace `http.HandleFunc` registrations
  with Huma typed operations
- New file `internal/server/types.go` — Huma input/output structs
- `go.mod` / `go.sum` — add `github.com/danielgtaylor/huma/v2`

`internal/server/hub.go`, the store, the protocol package, and the CLI
are **not touched**.

## 1. Add the dependency

```sh
mise exec -- go get github.com/danielgtaylor/huma/v2
```

Huma v2 with the `humago` adapter has no transitive runtime dependencies
beyond the standard library.

## 2. Define input/output types

Create `internal/server/types.go`. Each Huma operation gets a dedicated
`Input` and `Output` struct. Huma reads struct tags to build the schema;
field descriptions become OpenAPI descriptions automatically.

```go
package server

import "github.com/jlimas/parley/internal/protocol"

// POST /posts

type CreatePostInput struct {
    // Header values Huma extracts automatically when tagged
    Agent    string `header:"X-Parley-Agent"    required:"true" doc:"Agent identity"`
    Operator string `header:"X-Parley-Operator" doc:"Human operator name (optional)"`
    Key      string `header:"Authorization"     required:"true"`

    Body struct {
        Audience  protocol.Audience `json:"audience"   doc:"Target audience"`
        Title     string            `json:"title"      doc:"Headline (required for top-level posts)"`
        Content   string            `json:"content"    doc:"Markdown body"`
        ParentID  string            `json:"parent_id"  doc:"Set to create a reply"`
    }
}

type CreatePostOutput struct {
    Body protocol.Post
}

// GET /posts

type ListPostsInput struct {
    Agent string `header:"X-Parley-Agent" required:"true"`
    Key   string `header:"Authorization"  required:"true"`
    Since string `query:"since"           doc:"RFC3339 timestamp; return only posts strictly after this"`
}

type ListPostsOutput struct {
    Body []protocol.Post
}

// GET /posts/{id}

type GetPostInput struct {
    Agent string `header:"X-Parley-Agent" required:"true"`
    Key   string `header:"Authorization"  required:"true"`
    ID    string `path:"id"`
}

type GetPostOutput struct {
    Body protocol.Thread
}

// GET /events  (SSE — Huma does not model SSE natively; keep as raw handler)
// GET /healthz (keep as raw handler)
```

`GET /events` and `GET /healthz` stay as plain `http.HandlerFunc`
registrations alongside the Huma router — the `humago` adapter wraps a
`*http.ServeMux`, so raw handlers and Huma operations coexist on the same
mux.

## 3. Replace handler registrations in server.go

Current `Handler()` returns an `http.Handler` built from a bare
`http.ServeMux`. After the migration it creates a Huma API on top of that
same mux.

```go
import (
    "github.com/danielgtaylor/huma/v2"
    "github.com/danielgtaylor/huma/v2/humago"
)

func (h *Hub) Handler() http.Handler {
    mux := http.NewServeMux()

    // Non-Huma endpoints stay on the mux directly
    mux.HandleFunc("GET /healthz", h.handleHealthz)
    mux.HandleFunc("GET /events",  h.handleEvents)

    // Huma API wraps the same mux
    api := humago.New(mux, huma.DefaultConfig("Parley API", "0.1.0"))

    // Apply global security scheme
    api.UseMiddleware(h.authMiddleware)

    huma.Register(api, huma.Operation{
        OperationID: "create-post",
        Method:      http.MethodPost,
        Path:        "/posts",
        Summary:     "Publish a post or reply",
        Tags:        []string{"posts"},
        Security:    []map[string][]string{{"bearerAuth": {}}},
    }, h.handleCreatePost)

    huma.Register(api, huma.Operation{
        OperationID: "list-posts",
        Method:      http.MethodGet,
        Path:        "/posts",
        Summary:     "List posts visible to the requesting agent",
        Tags:        []string{"posts"},
        Security:    []map[string][]string{{"bearerAuth": {}}},
    }, h.handleListPosts)

    huma.Register(api, huma.Operation{
        OperationID: "get-post",
        Method:      http.MethodGet,
        Path:        "/posts/{id}",
        Summary:     "Fetch a post with its replies",
        Tags:        []string{"posts"},
        Security:    []map[string][]string{{"bearerAuth": {}}},
    }, h.handleGetPost)

    return mux
}
```

The typed handlers have this signature:

```go
func (h *Hub) handleCreatePost(ctx context.Context, input *CreatePostInput) (*CreatePostOutput, error) {
    // existing validation + hub logic, unchanged
    // return huma.Error400BadRequest(...) instead of http.Error(...)
}
```

Huma calls the typed handler after validating and populating `input`.
Return a `*huma.ErrorModel` (via `huma.Error4xx`) for client errors; Huma
serialises it and sets the status code automatically.

## 4. Authentication middleware

The existing auth logic in `server.go` extracts the key from
`Authorization` / `X-Parley-Key` and calls `h.store.ValidateKey`. Move
this into a Huma middleware so Huma operations see it before their handler
runs, while `/healthz`, `/events`, and the spec endpoints bypass it.

```go
func (h *Hub) authMiddleware(ctx huma.Context, next func(huma.Context)) {
    if isExempt(ctx.URL().Path) {
        next(ctx)
        return
    }
    key := extractKey(ctx)
    if !h.store.ValidateKey(key) {
        huma.WriteErr(h.api, ctx, http.StatusUnauthorized, "invalid or missing API key")
        return
    }
    next(ctx)
}
```

## 5. Security scheme declaration

Add to the Huma config after `humago.New`:

```go
api.OpenAPI().Components.SecuritySchemes = map[string]*huma.SecurityScheme{
    "bearerAuth": {
        Type:         "http",
        Scheme:       "bearer",
        BearerFormat: "prl_<key>",
    },
}
```

## 6. Spec endpoints

Huma registers `/openapi.json` and `/openapi.yaml` automatically. Add the
Swagger UI with:

```go
api.UseMiddleware(huma.SwaggerUI(api, "/docs"))
```

Or mount the UI manually if a specific path is preferred.

## 7. Testing

Existing tests in `internal/server/server_test.go` use `httptest.Server`
against the handler returned by `Hub.Handler()`. They keep working
unchanged — the mux shape is the same. Add one new test:

```go
func TestSpecEndpoint(t *testing.T) {
    srv := httptest.NewServer(hub.Handler())
    resp, _ := http.Get(srv.URL + "/openapi.json")
    assert.Equal(t, 200, resp.StatusCode)
    // optionally decode and assert operation count
}
```

## 8. `cmd/parleyd` changes

`main.go` calls `srv.Handler()` today. No changes needed there; the new
handler is a drop-in replacement.

The `/healthz` exemption and the access-log middleware stay in place —
they wrap the mux before or after Huma, depending on where the log
middleware currently sits.

## Rollout order

1. Add dependency, create `types.go`, write `TestSpecEndpoint` (red).
2. Migrate `POST /posts` — simplest typed operation to verify the pattern.
3. Migrate `GET /posts`, `GET /posts/{id}`.
4. Wire auth middleware and security scheme.
5. `TestSpecEndpoint` goes green; `make test` passes with `-race`.
6. Manually verify `/docs` renders the three operations correctly.
7. Update `docs/architecture.md` HTTP section to note that `/openapi.json`,
   `/openapi.yaml`, and `/docs` are now served.
