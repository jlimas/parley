package server

import "github.com/jlimas/parley/internal/protocol"

// CreatePostInput is the Huma input type for POST /posts.
// Pointer fields are optional in Huma's schema validation.
type CreatePostInput struct {
	Agent string `header:"X-Parley-Agent" doc:"Agent name (dev/no-auth fallback only; ignored when API key auth is active)"`
	Body  struct {
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
	Agent string `header:"X-Parley-Agent" doc:"Agent name (dev/no-auth fallback only; ignored when API key auth is active)"`
	Since string `query:"since"           doc:"RFC3339 timestamp; return only posts strictly after this"`
}

// ListPostsOutput is the Huma output type for GET /posts.
type ListPostsOutput struct {
	Body []protocol.Post
}

// GetPostInput is the Huma input type for GET /posts/{id}.
type GetPostInput struct {
	Agent string `header:"X-Parley-Agent" doc:"Agent name (dev/no-auth fallback only; ignored when API key auth is active)"`
	ID    string `path:"id"               doc:"Post ID"`
}

// GetPostOutput is the Huma output type for GET /posts/{id}.
type GetPostOutput struct {
	Body protocol.Thread
}

// MeInput is the Huma input type for GET /me.
// No fields needed; the client identity is derived from the API key in context.
type MeInput struct{}

// MeBody is the response body for GET /me.
type MeBody struct {
	ClientID    string `json:"client_id"    doc:"Short stable ID bound to the authenticated key"`
	DisplayName string `json:"display_name" doc:"Current display name for this client"`
	TenantID    string `json:"tenant_id"    doc:"Tenant the authenticated key belongs to"`
}

// MeOutput is the Huma output type for GET /me.
type MeOutput struct {
	Body MeBody
}

// RenameInput is the Huma input type for PATCH /me/name.
type RenameInput struct {
	Body struct {
		Name string `json:"name" doc:"New display name for this client"`
	}
}

// RenameOutput is the Huma output type for PATCH /me/name.
type RenameOutput struct {
	Body struct {
		ClientID    string `json:"client_id"`
		DisplayName string `json:"display_name"`
	}
}

// ClientItem represents one client in the GET /agents response.
type ClientItem struct {
	ID   string `json:"id"   doc:"Short stable client ID"`
	Name string `json:"name" doc:"Current display name"`
}

// ListAgentsInput is the Huma input type for GET /agents.
type ListAgentsInput struct {
	Agent string `header:"X-Parley-Agent" doc:"Agent name (dev/no-auth fallback only; ignored when API key auth is active)"`
}

// ListAgentsOutput is the Huma output type for GET /agents.
type ListAgentsOutput struct {
	Body []ClientItem
}
