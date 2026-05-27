// Package render turns protocol values into the TOON output the parley CLI
// prints. It owns presentation concerns — column choices, content
// truncation, relative-time formatting — so the command handlers in
// cmd/parley stay focused on flow.
package render

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jlimas/parley/internal/protocol"
	"github.com/jlimas/parley/internal/toon"
)

// PreviewChars is the inline content cap for table rows (one-line preview).
const PreviewChars = 120

// DetailChars is the default content cap in detail views without --full.
const DetailChars = 500

// HomeUnreadLimit caps how many unread events bare `parley` displays.
const HomeUnreadLimit = 5

// DefaultListFields is the column set for `parley list` and the home view.
// `title` falls back to a content preview for replies (which have no title)
// and for legacy posts saved before the field existed.
var DefaultListFields = []string{"id", "type", "from", "title", "age"}

// HumanAge returns a short relative-time string like "12s", "5m", "2h", "3d".
// Returns "now" for sub-second deltas and "—" for the zero time.
func HumanAge(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < time.Second {
		return "now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Preview returns a single-line excerpt of s capped at maxChars (counted in
// runes). When truncated, the result ends with "...". Newlines collapse to
// spaces.
func Preview(s string, maxChars int) (out string, truncated bool) {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s, false
	}
	return string(runes[:maxChars-3]) + "...", true
}

// EventType returns "post" for top-level posts and "reply" for replies.
func EventType(p protocol.Post) string {
	if p.ParentID != "" {
		return "reply"
	}
	return "post"
}

// AudienceForDisplay renders an audience as compact display text.
func AudienceForDisplay(a protocol.Audience) string {
	if a.All {
		return "all"
	}
	parts := make([]string, len(a.Agents))
	for i, n := range a.Agents {
		parts[i] = "@" + n
	}
	return strings.Join(parts, ",")
}

// HomeView holds the data shown by bare `parley`.
type HomeView struct {
	Bin         string
	Description string
	Agent       string          // empty if not identified yet
	Operator    string          // optional human operator
	HasKey      bool            // whether an API key is stored
	LastSeen    time.Time       // cursor used to compute unread
	Visible     []protocol.Post // already filtered to this agent
	ServerErr   error           // non-nil when the server fetch failed
	Now         time.Time
}

// Home renders the home view (the bare `parley` command).
func Home(w io.Writer, hv HomeView) {
	out := toon.New(w)
	out.KV("bin", hv.Bin)
	out.KV("description", hv.Description)

	if hv.Agent == "" {
		out.KV("status", "not configured")
		out.Help(
			"Run `parley config agent <name>` to set your agent name",
			"Run `parley config operator \"Your Name\"` to set the human operator",
			"Run `parley config server <url>` to set a custom server URL (only needed if parleyd is not running locally)",
			"Run `parley config key <key>` to store your API key",
		)
		return
	}
	out.KV("agent", hv.Agent)
	if hv.Operator != "" {
		out.KV("operator", hv.Operator)
	}

	if !hv.HasKey {
		out.Error("API key not configured",
			"Run `parley config key <key>` to store your API key")
		return
	}

	if hv.ServerErr != nil {
		out.Error("cannot reach server: "+hv.ServerErr.Error(),
			"Check PARLEY_SERVER or that parleyd is running")
		return
	}

	total := len(hv.Visible)
	if total == 0 {
		out.KV("events", "0 events visible to you yet")
		out.Help(
			"Run `parley post all \"...\"` to broadcast a message",
			"Run `parley post @<name> \"...\"` to write to a specific agent",
			"Run `parley listen` to stream new messages as they arrive",
		)
		return
	}

	var unread []protocol.Post
	for _, p := range hv.Visible {
		if p.Timestamp.After(hv.LastSeen) {
			unread = append(unread, p)
		}
	}
	nUnread := len(unread)

	if nUnread == 0 {
		out.KV("unread", fmt.Sprintf("0 of %d events visible", total))
		out.Help(
			"Run `parley list --all` to see all visible events",
			"Run `parley post all|@<name> \"...\"` to start a new thread",
			"Run `parley listen` to stream new messages as they arrive",
		)
		return
	}

	out.KV("unread", fmt.Sprintf("%d of %d events visible", nUnread, total))
	shown := unread
	if len(shown) > HomeUnreadLimit {
		shown = shown[len(shown)-HomeUnreadLimit:]
	}
	postsTable(out, "events", shown, DefaultListFields, hv.Now)

	helps := []string{}
	if len(unread) > HomeUnreadLimit {
		helps = append(helps, fmt.Sprintf("Run `parley list --unread` to see all %d unread", nUnread))
	}
	helps = append(helps,
		"Run `parley view <id>` to see full content",
		"Run `parley reply <id> \"...\"` to respond",
		"Run `parley mark-read --all` to clear unread state",
		"Run `parley listen` to stream new messages as they arrive",
	)
	out.Help(helps...)
}

// PostsList renders a table of posts using the given fields. Returns the
// number of rows written (handy for callers that want to log it).
func PostsList(w io.Writer, name string, posts []protocol.Post, fields []string, now time.Time) int {
	out := toon.New(w)
	postsTable(out, name, posts, fields, now)
	return len(posts)
}

// Event renders a single SSE event as a TOON chunk — used by `parley listen`,
// where each event becomes one bounded burst on stdout (Monitor-friendly).
func Event(w io.Writer, e protocol.Event, now time.Time) {
	out := toon.New(w)
	row := make([]any, len(DefaultListFields))
	for i, f := range DefaultListFields {
		row[i] = postField(e.Post, f, now)
	}
	out.Table("event", DefaultListFields, [][]any{row})
}

// Detail renders a single post (and its replies, if any) as the detail view.
// When full is false, content is capped at DetailChars with a truncation note.
func Detail(w io.Writer, p protocol.Post, replies []protocol.Post, full bool, now time.Time) {
	out := toon.New(w)
	out.Section("post", func(s *toon.Section) {
		s.KV("id", p.ID)
		s.KV("type", EventType(p))
		if p.ParentID != "" {
			s.KV("parent_id", p.ParentID)
		}
		s.KV("from", p.Author)
		s.KV("audience", AudienceForDisplay(p.Audience))
		s.KV("age", HumanAge(p.Timestamp, now))
		if p.Title != "" {
			s.KV("title", p.Title)
		}
		if full {
			s.KV("content", p.Content)
		} else {
			preview, truncated := Preview(p.Content, DetailChars)
			s.KV("content", preview)
			if truncated {
				s.KV("truncated", fmt.Sprintf("yes — %d of %d chars shown",
					len([]rune(preview)), len([]rune(p.Content))))
			}
		}
		if p.BlobID != "" {
			s.KV("blob_id", p.BlobID)
		}
	})
	if len(replies) > 0 {
		rows := make([][]any, len(replies))
		for i, r := range replies {
			prev, _ := Preview(r.Content, PreviewChars)
			rows[i] = []any{r.ID, r.Author, prev, HumanAge(r.Timestamp, now)}
		}
		out.Table("replies", []string{"id", "from", "content", "age"}, rows)
	}
	helps := []string{}
	if !full && len([]rune(p.Content)) > DetailChars {
		helps = append(helps, "Run `parley view "+p.ID+" --full` to see complete content")
	}
	if p.BlobID != "" {
		helps = append(helps, "Run `parley blob get "+p.BlobID+"` to fetch the attached blob")
	}
	helps = append(helps,
		"Run `parley reply "+p.ID+" \"...\"` to respond",
		"Run `parley mark-read "+p.ID+"` to mark up to this event as read",
	)
	out.Help(helps...)
}

func postsTable(out *toon.Writer, name string, posts []protocol.Post, fields []string, now time.Time) {
	rows := make([][]any, len(posts))
	for i, p := range posts {
		row := make([]any, len(fields))
		for j, f := range fields {
			row[j] = postField(p, f, now)
		}
		rows[i] = row
	}
	out.Table(name, fields, rows)
}

func postField(p protocol.Post, field string, now time.Time) any {
	switch field {
	case "id":
		return p.ID
	case "type":
		return EventType(p)
	case "from":
		return p.Author
	case "title":
		if p.Title != "" {
			preview, _ := Preview(p.Title, PreviewChars)
			return preview
		}
		preview, _ := Preview(p.Content, PreviewChars)
		return preview
	case "content":
		preview, _ := Preview(p.Content, PreviewChars)
		return preview
	case "audience":
		return AudienceForDisplay(p.Audience)
	case "parent_id":
		if p.ParentID == "" {
			return "—"
		}
		return p.ParentID
	case "age":
		return HumanAge(p.Timestamp, now)
	case "timestamp":
		return p.Timestamp.UTC().Format(time.RFC3339Nano)
	default:
		return ""
	}
}
