// Package protocol defines the wire format shared by the parley CLI and the
// parleyd server. Both binaries import this package to keep their view of an
// Event in sync.
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Audience defines who is notified about a Post. Exactly one of All or
// Agents should be set; an empty audience is invalid.
type Audience struct {
	All    bool     `json:"all,omitempty"`
	Agents []string `json:"agents,omitempty"`
}

// Includes reports whether the given agent name is part of this audience.
func (a Audience) Includes(agent string) bool {
	if a.All {
		return true
	}
	for _, name := range a.Agents {
		if name == agent {
			return true
		}
	}
	return false
}

// String renders the audience back into its CLI form ("all" or "@name").
func (a Audience) String() string {
	if a.All {
		return "all"
	}
	parts := make([]string, len(a.Agents))
	for i, name := range a.Agents {
		parts[i] = "@" + name
	}
	return strings.Join(parts, ",")
}

// ParseAudience parses a CLI audience token. Accepted forms: "all" or
// "@agentName".
func ParseAudience(s string) (Audience, error) {
	if s == "all" {
		return Audience{All: true}, nil
	}
	if strings.HasPrefix(s, "@") && len(s) > 1 {
		return Audience{Agents: []string{s[1:]}}, nil
	}
	return Audience{}, fmt.Errorf("invalid audience %q: expected \"all\" or \"@agentName\"", s)
}

// Post is a message on the board. A reply is just a Post with ParentID set;
// its audience is inherited from the parent.
type Post struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Author    string    `json:"author"`
	Audience  Audience  `json:"audience"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Event wraps a Post for the SSE stream. Type is "post" for top-level posts
// and "reply" for posts with a ParentID.
type Event struct {
	Type string `json:"type"`
	Post Post   `json:"post"`
}

// Thread bundles a post with its direct replies. Returned by GET /posts/{id}.
type Thread struct {
	Post    Post   `json:"post"`
	Replies []Post `json:"replies,omitempty"`
}

// AsJSON returns the event encoded as a single JSON object (no trailing
// newline). Useful for NDJSON output.
func (e Event) AsJSON() ([]byte, error) {
	return json.Marshal(e)
}
