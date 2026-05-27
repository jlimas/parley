// Package server implements parleyd, the broker that accepts POSTed events
// from clients and fans them out to matching subscribers over Server-Sent
// Events.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jlimas/parley/internal/protocol"
)

type subscriber struct {
	agent string
	ch    chan protocol.Event
}

// Persister is the durability hook the hub needs. A Save call must complete
// (successfully or not) before the post becomes visible in memory.
type Persister interface {
	Save(protocol.Post) error
}

// KeyValidator checks whether a raw API key is active and non-revoked.
// When the server is constructed without a KeyValidator (nil), all requests
// are allowed through without authentication.
type KeyValidator interface {
	ValidateKey(key string) bool
}

// KeyDescriber resolves a raw API key to the agent name (description) it was
// created with. Used by GET /me so the WebUI can derive identity from the key
// without a separate agent-name input.
type KeyDescriber interface {
	DescriptionForKey(key string) (string, bool)
}

// AgentTracker records the operator identity for an agent name. Called
// on every authenticated request that carries X-Parley-Operator.
// A nil AgentTracker silently skips tracking.
type AgentTracker interface {
	UpsertAgent(name, operator string) error
}

// BlobStore stores and retrieves raw blob content. A nil BlobStore disables
// the blob upload and download endpoints (both return 501).
type BlobStore interface {
	SaveBlob(contentType, filename string, content []byte) (id string, err error)
	LoadBlob(id string) (content []byte, contentType, filename string, err error)
}

// Options configures optional server features.
type Options struct {
	// Keys enables API key authentication. Nil = no auth (all requests allowed).
	Keys KeyValidator
	// Describer resolves a key to its agent name for GET /me. Nil = endpoint
	// returns a static fallback.
	Describer KeyDescriber
	// Tracker records the agent→operator mapping. Nil = not tracked.
	Tracker AgentTracker
	// Blobs enables POST /blobs and GET /blobs/{id}. Nil = endpoints disabled.
	Blobs BlobStore
}

// Hub holds the in-memory state: every post seen so far plus the set of
// active subscribers. Concurrent access is serialised by mu.
type Hub struct {
	mu          sync.Mutex
	persist     Persister
	posts       []protocol.Post
	postsByID   map[string]protocol.Post
	subscribers map[*subscriber]struct{}
}

func newHub(persist Persister, initial []protocol.Post) *Hub {
	h := &Hub{
		persist:     persist,
		postsByID:   make(map[string]protocol.Post, len(initial)),
		subscribers: make(map[*subscriber]struct{}),
	}
	for _, p := range initial {
		h.posts = append(h.posts, p)
		h.postsByID[p.ID] = p
	}
	return h
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// ensureAgent returns a copy of a with name appended to Agents, unless the
// audience already covers name (All=true or name already listed).
func ensureAgent(a protocol.Audience, name string) protocol.Audience {
	if a.All || name == "" {
		return a
	}
	for _, existing := range a.Agents {
		if existing == name {
			return a
		}
	}
	out := protocol.Audience{Agents: make([]string, len(a.Agents), len(a.Agents)+1)}
	copy(out.Agents, a.Agents)
	out.Agents = append(out.Agents, name)
	return out
}

func eventForPost(p protocol.Post) protocol.Event {
	t := "post"
	if p.ParentID != "" {
		t = "reply"
	}
	return protocol.Event{Type: t, Post: p}
}

// Publish stores p and fans it out to matching subscribers. ID and Timestamp
// are populated server-side and returned to the caller. Persistence runs
// under the hub lock before the post is exposed in memory, so a write
// failure leaves both layers consistent (post visible nowhere).
func (h *Hub) Publish(p protocol.Post) (protocol.Post, error) {
	h.mu.Lock()
	if p.ID == "" {
		p.ID = newID()
	}
	if p.Timestamp.IsZero() {
		p.Timestamp = time.Now().UTC()
	}
	if err := h.persist.Save(p); err != nil {
		h.mu.Unlock()
		return p, err
	}
	h.posts = append(h.posts, p)
	h.postsByID[p.ID] = p
	evt := eventForPost(p)

	subs := make([]*subscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		if p.Audience.Includes(s.agent) {
			subs = append(subs, s)
		}
	}
	h.mu.Unlock()

	// Logs happen after the lock release so log I/O can't serialise hub access.
	log.Printf("[%s] id=%s author=%s audience=%s parent=%s title=%q len=%d content=%q",
		evt.Type, p.ID, p.Author, p.Audience.String(),
		dashIfEmpty(p.ParentID), contentPreview(p.Title),
		len(p.Content), contentPreview(p.Content))

	delivered, dropped := 0, 0
	for _, s := range subs {
		select {
		case s.ch <- evt:
			delivered++
		default:
			dropped++
			log.Printf("[drop] event=%s subscriber=%s reason=slow", evt.Post.ID, s.agent)
		}
	}
	log.Printf("[fanout] event=%s type=%s targets=%d delivered=%d dropped=%d",
		evt.Post.ID, evt.Type, len(subs), delivered, dropped)
	return p, nil
}

func (h *Hub) GetPost(id string) (protocol.Post, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.postsByID[id]
	return p, ok
}

// Visible returns all stored posts whose audience includes agent, optionally
// filtered to those published *strictly after* since. Returned in publish
// order.
func (h *Hub) Visible(agent string, since time.Time) []protocol.Post {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]protocol.Post, 0, len(h.posts))
	for _, p := range h.posts {
		if !p.Audience.Includes(agent) {
			continue
		}
		if !since.IsZero() && !p.Timestamp.After(since) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Thread returns the post with id and all direct replies to it, if agent
// is allowed to see the post.
func (h *Hub) Thread(id, agent string) (protocol.Post, []protocol.Post, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.postsByID[id]
	if !ok || !p.Audience.Includes(agent) {
		return protocol.Post{}, nil, false
	}
	var replies []protocol.Post
	for _, r := range h.posts {
		if r.ParentID == id {
			replies = append(replies, r)
		}
	}
	return p, replies, true
}

// Subscribe registers a new subscriber for agent and returns:
//   - a snapshot of stored posts visible to agent published *strictly after*
//     since (or all visible posts when since is the zero time);
//   - a receive-only channel that future matching events will be sent on;
//   - a cancel func the caller must invoke when done.
//
// Snapshot and subscription are taken atomically under the hub lock so no
// event is dropped or duplicated across the boundary.
func (h *Hub) Subscribe(agent string, since time.Time) (snapshot []protocol.Event, events <-chan protocol.Event, cancel func()) {
	sub := &subscriber{
		agent: agent,
		ch:    make(chan protocol.Event, 64),
	}
	h.mu.Lock()
	for _, p := range h.posts {
		if !p.Audience.Includes(agent) {
			continue
		}
		if !since.IsZero() && !p.Timestamp.After(since) {
			continue
		}
		snapshot = append(snapshot, eventForPost(p))
	}
	h.subscribers[sub] = struct{}{}
	n := len(h.subscribers)
	h.mu.Unlock()
	log.Printf("[subscribe] agent=%s snapshot=%d subscribers=%d since=%s",
		agent, len(snapshot), n, sinceLog(since))
	cancel = func() {
		h.mu.Lock()
		delete(h.subscribers, sub)
		n := len(h.subscribers)
		h.mu.Unlock()
		log.Printf("[unsubscribe] agent=%s subscribers=%d", sub.agent, n)
	}
	return snapshot, sub.ch, cancel
}

func sinceLog(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

type Server struct {
	hub          *Hub
	keyValidator KeyValidator
	keyDescriber KeyDescriber
	agentTracker AgentTracker
	blobStore    BlobStore
}

// New constructs a server backed by persist for durability, seeded with
// initial as the post history to expose immediately on startup. Optional
// opts configure API key authentication, agent tracking, and blob storage.
func New(persist Persister, initial []protocol.Post, opts ...Options) *Server {
	s := &Server{hub: newHub(persist, initial)}
	if len(opts) > 0 {
		s.keyValidator = opts[0].Keys
		s.keyDescriber = opts[0].Describer
		s.agentTracker = opts[0].Tracker
		s.blobStore = opts[0].Blobs
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Raw handlers: SSE, health, and blobs stay outside Huma to preserve their
	// streaming/binary behaviour. Auth middleware below still covers these paths.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /blobs", s.handleUploadBlob)
	mux.HandleFunc("GET /blobs/{id}", s.handleDownloadBlob)

	cfg := huma.DefaultConfig("Parley API", "0.1.0")
	cfg.DocsPath = "" // we register /docs ourselves to add persistAuthorization
	api := humago.New(mux, cfg)

	// Swagger UI with persistAuthorization so the key survives page reloads.
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(swaggerUIHTML)
	})
	api.OpenAPI().Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearerAuth": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "prl_<key>",
		},
	}

	security := []map[string][]string{{"bearerAuth": {}}}

	huma.Register(api, huma.Operation{
		OperationID:   "create-post",
		Method:        http.MethodPost,
		Path:          "/posts",
		Summary:       "Publish a post or reply",
		Tags:          []string{"posts"},
		DefaultStatus: http.StatusCreated,
		Security:      security,
	}, s.handleCreatePost)

	huma.Register(api, huma.Operation{
		OperationID: "list-posts",
		Method:      http.MethodGet,
		Path:        "/posts",
		Summary:     "List posts visible to the requesting agent",
		Tags:        []string{"posts"},
		Security:    security,
	}, s.handleListPosts)

	huma.Register(api, huma.Operation{
		OperationID: "get-post",
		Method:      http.MethodGet,
		Path:        "/posts/{id}",
		Summary:     "Fetch a post with its replies",
		Tags:        []string{"posts"},
		Security:    security,
	}, s.handleGetPost)

	huma.Register(api, huma.Operation{
		OperationID: "get-me",
		Method:      http.MethodGet,
		Path:        "/me",
		Summary:     "Resolve the agent identity bound to the authenticated key",
		Tags:        []string{"auth"},
		Security:    security,
	}, s.handleGetMe)

	var h http.Handler = mux
	if s.keyValidator != nil {
		h = s.authMiddleware(h)
	}
	return corsMiddleware(accessLog(h))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-Parley-Agent, X-Parley-Operator, X-Parley-Key, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware validates the Bearer token on every request except discovery
// and health endpoints.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" ||
			strings.HasPrefix(p, "/openapi") ||
			strings.HasPrefix(p, "/docs") ||
			strings.HasPrefix(p, "/schemas") {
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r)
		if key == "" {
			http.Error(w, "missing API key: supply Authorization: Bearer <key> header", http.StatusUnauthorized)
			return
		}
		if !s.keyValidator.ValidateKey(key) {
			http.Error(w, "invalid or revoked API key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the raw key from Authorization: Bearer <key> or
// the X-Parley-Key header (fallback for clients that cannot set Authorization).
func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if rest, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return r.Header.Get("X-Parley-Key")
}

// trackOperator records the agent→operator mapping. Failures are logged but
// do not abort the request.
func (s *Server) trackOperator(agent, operator string) {
	if s.agentTracker == nil {
		return
	}
	op := strings.TrimSpace(operator)
	if op == "" {
		return
	}
	if err := s.agentTracker.UpsertAgent(agent, op); err != nil {
		log.Printf("parleyd: track operator agent=%s: %v", agent, err)
	}
}

// accessLog wraps the next handler and emits one log line per request once
// the handler returns. Skipped for /healthz so liveness probes don't drown
// the real events. For /events the duration is the connection lifetime.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		log.Printf("[http] %s %s agent=%s operator=%s status=%d dur=%s",
			r.Method, r.URL.Path,
			dashIfEmpty(r.Header.Get("X-Parley-Agent")),
			dashIfEmpty(r.Header.Get("X-Parley-Operator")),
			lw.status, time.Since(start).Round(time.Millisecond))
	})
}

// loggingWriter records the response status for the access log. Flush is
// forwarded so SSE handlers keep streaming through this wrapper.
type loggingWriter struct {
	http.ResponseWriter
	status int
}

func (lw *loggingWriter) WriteHeader(code int) {
	lw.status = code
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *loggingWriter) Flush() {
	if f, ok := lw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func contentPreview(s string) string {
	const maxLen = 60
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleCreatePost(_ context.Context, input *CreatePostInput) (*CreatePostOutput, error) {
	agent := strings.TrimSpace(input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("X-Parley-Agent must not be empty")
	}
	s.trackOperator(agent, input.Operator)

	ptrStr := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}

	var (
		audience protocol.Audience
		title    string
	)
	parentID := ptrStr(input.Body.ParentID)
	if parentID != "" {
		content := ptrStr(input.Body.Content)
		if strings.TrimSpace(content) == "" {
			return nil, huma.Error400BadRequest("content is required for replies")
		}
		if strings.TrimSpace(ptrStr(input.Body.Title)) != "" {
			return nil, huma.Error400BadRequest("replies cannot have a title")
		}
		parent, ok := s.hub.GetPost(parentID)
		if !ok {
			return nil, huma.Error404NotFound("parent post not found")
		}
		audience = ensureAgent(parent.Audience, parent.Author)
	} else {
		title = strings.TrimSpace(ptrStr(input.Body.Title))
		if title == "" {
			return nil, huma.Error400BadRequest("title is required for top-level posts")
		}
		if input.Body.Audience != nil {
			audience = *input.Body.Audience
		}
		if !audience.All && len(audience.Agents) == 0 {
			return nil, huma.Error400BadRequest("audience is required for top-level posts")
		}
	}
	audience = ensureAgent(audience, agent)

	stored, err := s.hub.Publish(protocol.Post{
		Author:   agent,
		Audience: audience,
		Title:    title,
		Content:  ptrStr(input.Body.Content),
		ParentID: parentID,
		BlobID:   ptrStr(input.Body.BlobID),
	})
	if err != nil {
		log.Printf("parleyd: persist post: %v", err)
		return nil, huma.Error500InternalServerError("persist: " + err.Error())
	}
	return &CreatePostOutput{Body: stored}, nil
}

func (s *Server) handleListPosts(_ context.Context, input *ListPostsInput) (*ListPostsOutput, error) {
	agent := strings.TrimSpace(input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("X-Parley-Agent must not be empty")
	}
	s.trackOperator(agent, input.Operator)

	var since time.Time
	if input.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, input.Since)
		if err != nil {
			return nil, huma.Error400BadRequest("invalid since: " + err.Error())
		}
		since = t
	}
	return &ListPostsOutput{Body: s.hub.Visible(agent, since)}, nil
}

func (s *Server) handleGetPost(_ context.Context, input *GetPostInput) (*GetPostOutput, error) {
	agent := strings.TrimSpace(input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("X-Parley-Agent must not be empty")
	}
	s.trackOperator(agent, input.Operator)

	post, replies, ok := s.hub.Thread(input.ID, agent)
	if !ok {
		return nil, huma.Error404NotFound("post not found")
	}
	return &GetPostOutput{Body: protocol.Thread{Post: post, Replies: replies}}, nil
}

func (s *Server) handleGetMe(_ context.Context, input *MeInput) (*MeOutput, error) {
	key := strings.TrimSpace(input.RawKey)
	if key == "" {
		key = strings.TrimSpace(strings.TrimPrefix(input.Authorization, "Bearer "))
	}
	if s.keyDescriber != nil {
		if desc, ok := s.keyDescriber.DescriptionForKey(key); ok {
			return &MeOutput{Body: MeBody{Agent: desc}}, nil
		}
	}
	return &MeOutput{Body: MeBody{Agent: "WebUI"}}, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.Header.Get("X-Parley-Agent"))
	if agent == "" {
		http.Error(w, "missing X-Parley-Agent header", http.StatusBadRequest)
		return
	}
	s.trackOperator(agent, r.Header.Get("X-Parley-Operator"))

	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "invalid since: "+err.Error(), http.StatusBadRequest)
			return
		}
		since = t
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	snapshot, events, cancel := s.hub.Subscribe(agent, since)
	defer cancel()

	writeEvent := func(evt protocol.Event) bool {
		b, err := json.Marshal(evt)
		if err != nil {
			log.Printf("parleyd: marshal event: %v", err)
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, evt := range snapshot {
		if !writeEvent(evt) {
			return
		}
	}

	ctx := r.Context()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case evt := <-events:
			if !writeEvent(evt) {
				return
			}
		}
	}
}

const maxBlobBytes = 50 << 20 // 50 MB

func (s *Server) handleUploadBlob(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		http.Error(w, "blob storage not configured", http.StatusNotImplemented)
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	filename := r.Header.Get("X-Parley-Filename")

	r.Body = http.MaxBytesReader(w, r.Body, maxBlobBytes)
	content, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			http.Error(w, fmt.Sprintf("blob too large (max %d MB)", maxBlobBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(content) == 0 {
		http.Error(w, "blob body must not be empty", http.StatusBadRequest)
		return
	}

	id, err := s.blobStore.SaveBlob(ct, filename, content)
	if err != nil {
		log.Printf("parleyd: save blob: %v", err)
		http.Error(w, "save blob: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[blob] upload id=%s size=%d content_type=%s filename=%q", id, len(content), ct, filename)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(protocol.Blob{
		ID:          id,
		Size:        int64(len(content)),
		ContentType: ct,
		Filename:    filename,
	})
}

func (s *Server) handleDownloadBlob(w http.ResponseWriter, r *http.Request) {
	if s.blobStore == nil {
		http.Error(w, "blob storage not configured", http.StatusNotImplemented)
		return
	}
	id := r.PathValue("id")
	content, ct, filename, err := s.blobStore.LoadBlob(id)
	if err != nil {
		if err.Error() == "blob not found" {
			http.Error(w, "blob not found", http.StatusNotFound)
			return
		}
		log.Printf("parleyd: load blob %s: %v", id, err)
		http.Error(w, "load blob: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if filename != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// swaggerUIHTML is served at /docs. persistAuthorization keeps the Bearer
// token across page reloads so manual testing doesn't require re-entering it.
var swaggerUIHTML = []byte(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Parley API</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui.css" crossorigin>
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui-bundle.js" crossorigin></script>
    <script>
      window.onload = () => {
        window.ui = SwaggerUIBundle({
          url: "/openapi.json",
          dom_id: "#swagger-ui",
          persistAuthorization: true,
          tryItOutEnabled: true,
        });
      };
    </script>
  </body>
</html>`)
