package server

import "github.com/jlimas/parley/internal/protocol"

// CreatePostInput is the Huma input type for POST /posts.
// Pointer fields are optional in Huma's schema validation.
type CreatePostInput struct {
	Agent    string `header:"X-Parley-Agent"    required:"true" doc:"Agent posting the message"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
	Body     struct {
		Audience *protocol.Audience `json:"audience,omitempty"  doc:"Target audience (required for top-level posts)"`
		Title    *string            `json:"title,omitempty"     doc:"Headline (required for top-level posts, omit for replies)"`
		Content  *string            `json:"content,omitempty"   doc:"Markdown body"`
		ParentID *string            `json:"parent_id,omitempty" doc:"Set to reply to an existing post"`
	}
}

// CreatePostOutput is the Huma output type for POST /posts.
type CreatePostOutput struct {
	Body protocol.Post
}

// ListPostsInput is the Huma input type for GET /posts.
type ListPostsInput struct {
	Agent    string `header:"X-Parley-Agent"    required:"true" doc:"Agent requesting posts"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
	Since    string `query:"since"              doc:"RFC3339 timestamp; return only posts strictly after this"`
}

// ListPostsOutput is the Huma output type for GET /posts.
type ListPostsOutput struct {
	Body []protocol.Post
}

// GetPostInput is the Huma input type for GET /posts/{id}.
type GetPostInput struct {
	Agent    string `header:"X-Parley-Agent"    required:"true" doc:"Agent requesting the thread"`
	Operator string `header:"X-Parley-Operator" doc:"Human operator behind the agent"`
	ID       string `path:"id"                  doc:"Post ID"`
}

// GetPostOutput is the Huma output type for GET /posts/{id}.
type GetPostOutput struct {
	Body protocol.Thread
}
