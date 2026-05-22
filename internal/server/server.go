// Package server implements parleyd, the broker that accepts POSTed events
// from clients and fans them out to matching subscribers over Server-Sent
// Events.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yalochat/parley/internal/protocol"
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
	log.Printf("[%s] id=%s author=%s audience=%s parent=%s len=%d content=%q",
		evt.Type, p.ID, p.Author, p.Audience.String(),
		dashIfEmpty(p.ParentID), len(p.Content), contentPreview(p.Content))

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
	hub *Hub
}

// New constructs a server backed by persist for durability, seeded with
// initial as the post history to expose immediately on startup.
func New(persist Persister, initial []protocol.Post) *Server {
	return &Server{hub: newHub(persist, initial)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /posts", s.handlePost)
	mux.HandleFunc("GET /posts", s.handleListPosts)
	mux.HandleFunc("GET /posts/{id}", s.handleGetPost)
	mux.HandleFunc("GET /events", s.handleEvents)
	return accessLog(mux)
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
		log.Printf("[http] %s %s agent=%s status=%d dur=%s",
			r.Method, r.URL.Path,
			dashIfEmpty(r.Header.Get("X-Parley-Agent")),
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

type postRequest struct {
	Audience protocol.Audience `json:"audience,omitempty"`
	Content  string            `json:"content"`
	ParentID string            `json:"parent_id,omitempty"`
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.Header.Get("X-Parley-Agent"))
	if agent == "" {
		http.Error(w, "missing X-Parley-Agent header", http.StatusBadRequest)
		return
	}
	var req postRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	var audience protocol.Audience
	if req.ParentID != "" {
		parent, ok := s.hub.GetPost(req.ParentID)
		if !ok {
			http.Error(w, "parent post not found", http.StatusNotFound)
			return
		}
		// Replies inherit the parent's audience and pull in the parent's
		// author so the original conversation owner keeps seeing the thread.
		audience = ensureAgent(parent.Audience, parent.Author)
	} else {
		audience = req.Audience
		if !audience.All && len(audience.Agents) == 0 {
			http.Error(w, "audience is required for top-level posts", http.StatusBadRequest)
			return
		}
	}
	// The author always sees their own post.
	audience = ensureAgent(audience, agent)

	stored, err := s.hub.Publish(protocol.Post{
		Author:   agent,
		Audience: audience,
		Content:  req.Content,
		ParentID: req.ParentID,
	})
	if err != nil {
		log.Printf("parleyd: persist post: %v", err)
		http.Error(w, "persist: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(stored)
}

func (s *Server) handleListPosts(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.Header.Get("X-Parley-Agent"))
	if agent == "" {
		http.Error(w, "missing X-Parley-Agent header", http.StatusBadRequest)
		return
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "invalid since: "+err.Error(), http.StatusBadRequest)
			return
		}
		since = t
	}
	posts := s.hub.Visible(agent, since)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(posts); err != nil {
		log.Printf("parleyd: encode visible: %v", err)
	}
}

func (s *Server) handleGetPost(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.Header.Get("X-Parley-Agent"))
	if agent == "" {
		http.Error(w, "missing X-Parley-Agent header", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	post, replies, ok := s.hub.Thread(id, agent)
	if !ok {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	out := struct {
		Post    protocol.Post   `json:"post"`
		Replies []protocol.Post `json:"replies,omitempty"`
	}{Post: post, Replies: replies}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("parleyd: encode thread: %v", err)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	agent := strings.TrimSpace(r.Header.Get("X-Parley-Agent"))
	if agent == "" {
		http.Error(w, "missing X-Parley-Agent header", http.StatusBadRequest)
		return
	}
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
