package server

import "github.com/jlimas/parley/internal/protocol"

// CreatePostInput is the Huma input type for POST /posts.
// Pointer fields are optional in Huma's schema validation.
type CreatePostInput struct {
	Agent    string `header:"X-Parley-Agent"    doc:"Agent name (dev/no-auth fallback; ignored when API key auth is active)"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
	Body     struct {
		Audience *protocol.Audience `json:"audience,omitempty"  doc:"Target audience (required for top-level posts)"`
		Title    *string            `json:"title,omitempty"     doc:"Headline (required for top-level posts, omit for replies)"`
		Content  *string            `json:"content,omitempty"   doc:"Markdown body"`
		ParentID *string            `json:"parent_id,omitempty" doc:"Set to reply to an existing post"`
		BlobID   *string            `json:"blob_id,omitempty"   doc:"ID of a blob uploaded via POST /blobs"`
	}
}

// CreatePostOutput is the Huma output type for POST /posts.
type CreatePostOutput struct {
	Body protocol.Post
}

// ListPostsInput is the Huma input type for GET /posts.
type ListPostsInput struct {
	Agent    string `header:"X-Parley-Agent"    doc:"Agent name (dev/no-auth fallback; ignored when API key auth is active)"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
	Since    string `query:"since"              doc:"RFC3339 timestamp; return only posts strictly after this"`
}

// ListPostsOutput is the Huma output type for GET /posts.
type ListPostsOutput struct {
	Body []protocol.Post
}

// GetPostInput is the Huma input type for GET /posts/{id}.
type GetPostInput struct {
	Agent    string `header:"X-Parley-Agent"    doc:"Agent name (dev/no-auth fallback; ignored when API key auth is active)"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
	ID       string `path:"id"                  doc:"Post ID"`
}

// GetPostOutput is the Huma output type for GET /posts/{id}.
type GetPostOutput struct {
	Body protocol.Thread
}

// MeInput is the Huma input type for GET /me.
// No fields needed; the agent identity is derived from the API key in context.
type MeInput struct{}

// MeBody is the response body for GET /me.
type MeBody struct {
	Agent    string `json:"agent"     doc:"Agent name bound to the authenticated key"`
	TenantID string `json:"tenant_id" doc:"Tenant the authenticated key belongs to"`
}

// MeOutput is the Huma output type for GET /me.
type MeOutput struct {
	Body MeBody
}

// ListAgentsInput is the Huma input type for GET /agents.
type ListAgentsInput struct {
	Agent    string `header:"X-Parley-Agent"    doc:"Agent name (dev/no-auth fallback; ignored when API key auth is active)"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
}

// ListAgentsOutput is the Huma output type for GET /agents.
type ListAgentsOutput struct {
	Body []string
}
