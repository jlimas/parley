// Package server implements parleyd, the broker that accepts POSTed events
// from clients and fans them out to matching subscribers over Server-Sent
// Events.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jlimas/parley/internal/protocol"
	"github.com/jlimas/parley/internal/store"
)

type subscriber struct {
	agent string
	ch    chan protocol.Event
}

// Persister is the durability hook the hub needs.
type Persister interface {
	Save(protocol.Post) error
	UpdatePost(p protocol.Post) error
	DeletePost(tenantID, id string) error
}

// Sentinel errors returned by Hub mutation methods.
var (
	ErrPostNotFound = errors.New("post not found")
	ErrNotOwner     = errors.New("not the author of this post")
	ErrHasReplies   = errors.New("cannot delete a post that has replies")
)

// KeyValidator checks whether a raw API key is active and non-revoked.
// When nil, all requests are allowed through without authentication.
type KeyValidator interface {
	ValidateKey(key string) bool
}

// KeyDescriber resolves a raw API key to its (tenantID, clientID, displayName) triple.
// Used by the auth middleware to derive identity from the key on every request.
type KeyDescriber interface {
	ClientForKey(key string) (tenantID, clientID, displayName string, ok bool)
}

// ClientRenamer updates the display name of a client within a tenant.
type ClientRenamer interface {
	RenameClient(tenantID, clientID, newName string) error
}

// ClientLister returns all active clients for a tenant.
type ClientLister interface {
	ListClients(tenantID string) ([]store.ClientRecord, error)
}

// BlobStore stores and retrieves raw blob content scoped to a tenant.
// A nil BlobStore disables the blob endpoints (both return 501).
type BlobStore interface {
	SaveBlob(tenantID, contentType, filename string, content []byte) (id string, err error)
	LoadBlob(tenantID, id string) (content []byte, contentType, filename string, err error)
	BlobFilename(tenantID, id string) (string, error)
}

// Options configures optional server features.
type Options struct {
	Keys      KeyValidator
	Describer KeyDescriber
	Renamer   ClientRenamer
	Lister    ClientLister
	Blobs     BlobStore
}

// -- Context keys --

type ctxAgentKey struct{}       // holds clientID (short base-36 ID)
type ctxDisplayNameKey struct{} // holds current display name for the client
type ctxTenantKey struct{}

func agentFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxAgentKey{}).(string)
	return v
}

func displayNameFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxDisplayNameKey{}).(string)
	return v
}

func tenantFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxTenantKey{}).(string)
	return v
}

// resolveAgent returns the key-derived clientID from context; falls back to the
// X-Parley-Agent header only in no-auth dev mode.
func resolveAgent(ctx context.Context, header string) string {
	if v := agentFromCtx(ctx); v != "" {
		return v
	}
	return strings.TrimSpace(header)
}

// -- Hub (per-tenant in-memory board) --

// Hub holds the in-memory state for one tenant: every post seen so far plus
// the set of active subscribers. Concurrent access is serialised by mu.
type Hub struct {
	tenantID    string
	mu          sync.Mutex
	persist     Persister
	posts       []protocol.Post
	postsByID   map[string]protocol.Post
	subscribers map[*subscriber]struct{}
}

func newHub(tenantID string, persist Persister, initial []protocol.Post) *Hub {
	h := &Hub{
		tenantID:    tenantID,
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

// Publish stores p in this tenant's hub and fans it out to matching
// subscribers. ID and Timestamp are populated server-side.
func (h *Hub) Publish(p protocol.Post) (protocol.Post, error) {
	h.mu.Lock()
	if p.ID == "" {
		p.ID = newID()
	}
	if p.Timestamp.IsZero() {
		p.Timestamp = time.Now().UTC()
	}
	p.TenantID = h.tenantID
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

// Update edits the content (and, for top-level posts, optionally the title)
// of an existing post. Only the author of the post may update it.
func (h *Hub) Update(id, agentID, newContent, newTitle string) (protocol.Post, error) {
	h.mu.Lock()
	p, ok := h.postsByID[id]
	if !ok {
		h.mu.Unlock()
		return protocol.Post{}, ErrPostNotFound
	}
	if p.Author != agentID {
		h.mu.Unlock()
		return protocol.Post{}, ErrNotOwner
	}
	p.Content = newContent
	if p.ParentID == "" && newTitle != "" {
		p.Title = newTitle
	}
	now := time.Now().UTC()
	p.EditedAt = &now
	if err := h.persist.UpdatePost(p); err != nil {
		h.mu.Unlock()
		return protocol.Post{}, err
	}
	for i, existing := range h.posts {
		if existing.ID == id {
			h.posts[i] = p
			break
		}
	}
	h.postsByID[id] = p
	evt := protocol.Event{Type: "update", Post: p}
	subs := make([]*subscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		if p.Audience.Includes(s.agent) {
			subs = append(subs, s)
		}
	}
	h.mu.Unlock()
	log.Printf("[update] id=%s author=%s", p.ID, p.Author)
	for _, s := range subs {
		select {
		case s.ch <- evt:
		default:
			log.Printf("[drop] event=%s subscriber=%s reason=slow", evt.Post.ID, s.agent)
		}
	}
	return p, nil
}

// Delete removes a post from the hub. Only the author may delete their own
// post. Top-level posts with replies cannot be deleted.
func (h *Hub) Delete(id, agentID string) (protocol.Post, error) {
	h.mu.Lock()
	p, ok := h.postsByID[id]
	if !ok {
		h.mu.Unlock()
		return protocol.Post{}, ErrPostNotFound
	}
	if p.Author != agentID {
		h.mu.Unlock()
		return protocol.Post{}, ErrNotOwner
	}
	if p.ParentID == "" {
		for _, r := range h.posts {
			if r.ParentID == id {
				h.mu.Unlock()
				return protocol.Post{}, ErrHasReplies
			}
		}
	}
	if err := h.persist.DeletePost(h.tenantID, id); err != nil {
		h.mu.Unlock()
		return protocol.Post{}, err
	}
	delete(h.postsByID, id)
	for i, existing := range h.posts {
		if existing.ID == id {
			h.posts = append(h.posts[:i], h.posts[i+1:]...)
			break
		}
	}
	evt := protocol.Event{Type: "delete", Post: p}
	subs := make([]*subscriber, 0, len(h.subscribers))
	for s := range h.subscribers {
		if p.Audience.Includes(s.agent) {
			subs = append(subs, s)
		}
	}
	h.mu.Unlock()
	log.Printf("[delete] id=%s author=%s", p.ID, p.Author)
	for _, s := range subs {
		select {
		case s.ch <- evt:
		default:
			log.Printf("[drop] event=%s subscriber=%s reason=slow", evt.Post.ID, s.agent)
		}
	}
	return p, nil
}

// KnownAgents returns the deduplicated, sorted list of agent names that have
// appeared as authors or named audience members in any post in this hub.
func (h *Hub) KnownAgents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[string]struct{})
	for _, p := range h.posts {
		seen[p.Author] = struct{}{}
		if !p.Audience.All {
			for _, a := range p.Audience.Agents {
				seen[a] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

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
	log.Printf("[subscribe] tenant=%s agent=%s snapshot=%d subscribers=%d since=%s",
		h.tenantID, agent, len(snapshot), n, sinceLog(since))
	cancel = func() {
		h.mu.Lock()
		delete(h.subscribers, sub)
		n := len(h.subscribers)
		h.mu.Unlock()
		log.Printf("[unsubscribe] tenant=%s agent=%s subscribers=%d", h.tenantID, sub.agent, n)
	}
	return snapshot, sub.ch, cancel
}

func sinceLog(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// -- Server --

type Server struct {
	mu            sync.RWMutex
	hubs          map[string]*Hub // tenant_id → hub
	persist       Persister
	keyValidator  KeyValidator
	keyDescriber  KeyDescriber
	clientRenamer ClientRenamer
	clientLister  ClientLister
	blobStore     BlobStore
}

// New constructs a server backed by persist for durability, seeded with
// initialByTenant as the per-tenant post histories.
func New(persist Persister, initialByTenant map[string][]protocol.Post, opts ...Options) *Server {
	s := &Server{
		hubs:    make(map[string]*Hub),
		persist: persist,
	}
	for tenantID, posts := range initialByTenant {
		s.hubs[tenantID] = newHub(tenantID, persist, posts)
	}
	if len(opts) > 0 {
		s.keyValidator = opts[0].Keys
		s.keyDescriber = opts[0].Describer
		s.clientRenamer = opts[0].Renamer
		s.clientLister = opts[0].Lister
		s.blobStore = opts[0].Blobs
	}
	return s
}

// hubFor returns the hub for the given tenant, creating it if it does not
// exist yet (happens when the first post for a new tenant arrives at runtime).
func (s *Server) hubFor(tenantID string) *Hub {
	s.mu.RLock()
	h := s.hubs[tenantID]
	s.mu.RUnlock()
	if h != nil {
		return h
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h = s.hubs[tenantID]; h != nil {
		return h
	}
	h = newHub(tenantID, s.persist, nil)
	s.hubs[tenantID] = h
	return h
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /blobs", s.handleUploadBlob)
	mux.HandleFunc("GET /blobs/{id}", s.handleDownloadBlob)

	cfg := huma.DefaultConfig("Parley API", "0.1.0")
	cfg.DocsPath = ""
	api := humago.New(mux, cfg)

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
		Summary:     "Resolve the client identity bound to the authenticated key",
		Tags:        []string{"auth"},
		Security:    security,
	}, s.handleGetMe)

	huma.Register(api, huma.Operation{
		OperationID:   "rename-me",
		Method:        http.MethodPatch,
		Path:          "/me/name",
		Summary:       "Rename the authenticated client's display name",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusOK,
		Security:      security,
	}, s.handleRenameMe)

	huma.Register(api, huma.Operation{
		OperationID: "list-agents",
		Method:      http.MethodGet,
		Path:        "/agents",
		Summary:     "List all clients known to this tenant",
		Tags:        []string{"agents"},
		Security:    security,
	}, s.handleListAgents)

	huma.Register(api, huma.Operation{
		OperationID:   "update-post",
		Method:        http.MethodPatch,
		Path:          "/posts/{id}",
		Summary:       "Edit a post or reply (author only)",
		Tags:          []string{"posts"},
		DefaultStatus: http.StatusOK,
		Security:      security,
	}, s.handleUpdatePost)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-post",
		Method:        http.MethodDelete,
		Path:          "/posts/{id}",
		Summary:       "Delete a post or reply (author only; top-level posts with replies cannot be deleted)",
		Tags:          []string{"posts"},
		DefaultStatus: http.StatusNoContent,
		Security:      security,
	}, s.handleDeletePost)

	var h http.Handler = mux
	if s.keyValidator != nil {
		h = s.authMiddleware(h)
	}
	return corsMiddleware(accessLog(h))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-Parley-Key, Content-Type, X-Parley-Agent")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

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
		if s.keyDescriber != nil {
			tenantID, clientID, displayName, ok := s.keyDescriber.ClientForKey(key)
			if !ok || tenantID == "" || clientID == "" {
				http.Error(w, "API key has no associated tenant/client", http.StatusInternalServerError)
				return
			}
			ctx := context.WithValue(r.Context(), ctxTenantKey{}, tenantID)
			ctx = context.WithValue(ctx, ctxAgentKey{}, clientID)
			ctx = context.WithValue(ctx, ctxDisplayNameKey{}, displayName)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if rest, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return r.Header.Get("X-Parley-Key")
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		lw := &loggingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		log.Printf("[http] %s %s tenant=%s agent=%s status=%d dur=%s",
			r.Method, r.URL.Path,
			dashIfEmpty(tenantFromCtx(r.Context())),
			dashIfEmpty(agentFromCtx(r.Context())),
			lw.status, time.Since(start).Round(time.Millisecond))
	})
}

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

func (s *Server) handleCreatePost(ctx context.Context, input *CreatePostInput) (*CreatePostOutput, error) {
	agent := resolveAgent(ctx, input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	displayName := displayNameFromCtx(ctx)
	if displayName == "" {
		displayName = agent // dev mode fallback: use agent header value as display name
	}
	tenantID := tenantFromCtx(ctx)

	hub := s.hubFor(tenantID)

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
		parent, ok := hub.GetPost(parentID)
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

	blobID := ptrStr(input.Body.BlobID)
	blobName := ""
	if blobID != "" && s.blobStore != nil {
		if name, err := s.blobStore.BlobFilename(tenantID, blobID); err == nil {
			blobName = name
		}
	}
	stored, err := hub.Publish(protocol.Post{
		Author:     agent,
		AuthorName: displayName,
		Audience:   audience,
		Title:      title,
		Content:    ptrStr(input.Body.Content),
		ParentID:   parentID,
		BlobID:     blobID,
		BlobName:   blobName,
	})
	if err != nil {
		log.Printf("parleyd: persist post: %v", err)
		return nil, huma.Error500InternalServerError("persist: " + err.Error())
	}
	return &CreatePostOutput{Body: stored}, nil
}

func (s *Server) handleUpdatePost(ctx context.Context, input *UpdatePostInput) (*UpdatePostOutput, error) {
	agent := resolveAgent(ctx, input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	tenantID := tenantFromCtx(ctx)

	newContent := ""
	newTitle := ""
	if input.Body.Content != nil {
		newContent = *input.Body.Content
	}
	if input.Body.Title != nil {
		newTitle = *input.Body.Title
	}
	if newContent == "" && newTitle == "" {
		return nil, huma.Error400BadRequest("at least one of content or title must be provided")
	}

	updated, err := s.hubFor(tenantID).Update(input.ID, agent, newContent, newTitle)
	if err != nil {
		switch {
		case errors.Is(err, ErrPostNotFound):
			return nil, huma.Error404NotFound("post not found")
		case errors.Is(err, ErrNotOwner):
			return nil, huma.Error403Forbidden("only the author may edit this post")
		}
		log.Printf("parleyd: update post: %v", err)
		return nil, huma.Error500InternalServerError("update: " + err.Error())
	}
	return &UpdatePostOutput{Body: updated}, nil
}

func (s *Server) handleDeletePost(ctx context.Context, input *DeletePostInput) (*struct{}, error) {
	agent := resolveAgent(ctx, input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	tenantID := tenantFromCtx(ctx)

	if _, err := s.hubFor(tenantID).Delete(input.ID, agent); err != nil {
		switch {
		case errors.Is(err, ErrPostNotFound):
			return nil, huma.Error404NotFound("post not found")
		case errors.Is(err, ErrNotOwner):
			return nil, huma.Error403Forbidden("only the author may delete this post")
		case errors.Is(err, ErrHasReplies):
			return nil, huma.Error409Conflict("cannot delete a post that has replies")
		}
		log.Printf("parleyd: delete post: %v", err)
		return nil, huma.Error500InternalServerError("delete: " + err.Error())
	}
	return nil, nil
}

func (s *Server) handleListPosts(ctx context.Context, input *ListPostsInput) (*ListPostsOutput, error) {
	agent := resolveAgent(ctx, input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	tenantID := tenantFromCtx(ctx)

	var since time.Time
	if input.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, input.Since)
		if err != nil {
			return nil, huma.Error400BadRequest("invalid since: " + err.Error())
		}
		since = t
	}
	posts := s.hubFor(tenantID).Visible(agent, since)
	s.resolveAuthorNames(tenantID, posts)
	return &ListPostsOutput{Body: posts}, nil
}

func (s *Server) handleGetPost(ctx context.Context, input *GetPostInput) (*GetPostOutput, error) {
	agent := resolveAgent(ctx, input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	tenantID := tenantFromCtx(ctx)

	post, replies, ok := s.hubFor(tenantID).Thread(input.ID, agent)
	if !ok {
		return nil, huma.Error404NotFound("post not found")
	}
	all := append([]protocol.Post{post}, replies...)
	s.resolveAuthorNames(tenantID, all)
	return &GetPostOutput{Body: protocol.Thread{Post: all[0], Replies: all[1:]}}, nil
}

// resolveAuthorNames populates AuthorName on each post by looking up all
// client IDs for the tenant in one batch. Posts already having an AuthorName
// set (e.g. freshly published) are skipped.
func (s *Server) resolveAuthorNames(tenantID string, posts []protocol.Post) {
	if s.clientLister == nil || len(posts) == 0 {
		return
	}
	clients, err := s.clientLister.ListClients(tenantID)
	if err != nil {
		return
	}
	nameMap := make(map[string]string, len(clients))
	for _, c := range clients {
		nameMap[c.ClientID] = c.DisplayName
	}
	for i := range posts {
		if name, ok := nameMap[posts[i].Author]; ok {
			posts[i].AuthorName = name
		}
	}
}

func (s *Server) handleGetMe(ctx context.Context, _ *MeInput) (*MeOutput, error) {
	clientID := agentFromCtx(ctx)
	displayName := displayNameFromCtx(ctx)
	tenant := tenantFromCtx(ctx)
	if clientID == "" {
		clientID = "unknown"
	}
	return &MeOutput{Body: MeBody{ClientID: clientID, DisplayName: displayName, TenantID: tenant}}, nil
}

func (s *Server) handleRenameMe(ctx context.Context, input *RenameInput) (*RenameOutput, error) {
	clientID := agentFromCtx(ctx)
	if clientID == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	tenantID := tenantFromCtx(ctx)
	newName := strings.TrimSpace(input.Body.Name)
	if newName == "" {
		return nil, huma.Error400BadRequest("name must not be empty")
	}
	if strings.HasPrefix(newName, "-") || strings.ContainsAny(newName, " \t\n") {
		return nil, huma.Error400BadRequest("name must not start with '-' or contain whitespace")
	}
	if s.clientRenamer == nil {
		return nil, huma.Error501NotImplemented("rename not available")
	}
	if err := s.clientRenamer.RenameClient(tenantID, clientID, newName); err != nil {
		return nil, huma.Error500InternalServerError("rename: " + err.Error())
	}
	out := &RenameOutput{}
	out.Body.ClientID = clientID
	out.Body.DisplayName = newName
	return out, nil
}

func (s *Server) handleListAgents(ctx context.Context, input *ListAgentsInput) (*ListAgentsOutput, error) {
	agent := resolveAgent(ctx, input.Agent)
	if agent == "" {
		return nil, huma.Error400BadRequest("client identity required: authenticate with a valid API key")
	}
	tenantID := tenantFromCtx(ctx)
	if s.clientLister == nil {
		// fall back to hub-derived names (dev/no-auth mode)
		known := s.hubFor(tenantID).KnownAgents()
		items := make([]ClientItem, len(known))
		for i, n := range known {
			items[i] = ClientItem{ID: n, Name: n}
		}
		return &ListAgentsOutput{Body: items}, nil
	}
	clients, err := s.clientLister.ListClients(tenantID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list clients: " + err.Error())
	}
	items := make([]ClientItem, len(clients))
	for i, c := range clients {
		items[i] = ClientItem{ID: c.ClientID, Name: c.DisplayName}
	}
	return &ListAgentsOutput{Body: items}, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	agent := resolveAgent(r.Context(), r.Header.Get("X-Parley-Agent"))
	if agent == "" {
		http.Error(w, "client identity required: authenticate with a valid API key", http.StatusBadRequest)
		return
	}
	tenantID := tenantFromCtx(r.Context())

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

	snapshot, events, cancel := s.hubFor(tenantID).Subscribe(agent, since)
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
	tenantID := tenantFromCtx(r.Context())
	if tenantID == "" {
		http.Error(w, "tenant identity required", http.StatusBadRequest)
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

	id, err := s.blobStore.SaveBlob(tenantID, ct, filename, content)
	if err != nil {
		log.Printf("parleyd: save blob: %v", err)
		http.Error(w, "save blob: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[blob] upload tenant=%s id=%s size=%d content_type=%s filename=%q", tenantID, id, len(content), ct, filename)

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
	tenantID := tenantFromCtx(r.Context())
	id := r.PathValue("id")
	content, ct, filename, err := s.blobStore.LoadBlob(tenantID, id)
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
