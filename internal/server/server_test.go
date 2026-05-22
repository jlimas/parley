package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yalochat/parley/internal/protocol"
)

func TestMain(m *testing.M) {
	// The hub logs every publish; silence it so `go test -v` stays readable.
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// mockPersister records Save calls and can be configured to fail.
type mockPersister struct {
	mu    sync.Mutex
	saved []protocol.Post
	fail  error
}

func (m *mockPersister) Save(p protocol.Post) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.saved = append(m.saved, p)
	return nil
}

func (m *mockPersister) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.saved)
}

func TestEnsureAgent(t *testing.T) {
	cases := []struct {
		name string
		in   protocol.Audience
		add  string
		want protocol.Audience
	}{
		{
			name: "all leaves audience unchanged",
			in:   protocol.Audience{All: true},
			add:  "alice",
			want: protocol.Audience{All: true},
		},
		{
			name: "empty name is a no-op",
			in:   protocol.Audience{Agents: []string{"bob"}},
			add:  "",
			want: protocol.Audience{Agents: []string{"bob"}},
		},
		{
			name: "already-listed agent is a no-op",
			in:   protocol.Audience{Agents: []string{"alice", "bob"}},
			add:  "alice",
			want: protocol.Audience{Agents: []string{"alice", "bob"}},
		},
		{
			name: "new agent is appended",
			in:   protocol.Audience{Agents: []string{"alice"}},
			add:  "bob",
			want: protocol.Audience{Agents: []string{"alice", "bob"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ensureAgent(tc.in, tc.add)
			if got.All != tc.want.All {
				t.Errorf("All: got %v want %v", got.All, tc.want.All)
			}
			if len(got.Agents) != len(tc.want.Agents) {
				t.Fatalf("Agents length: got %v want %v", got.Agents, tc.want.Agents)
			}
			for i := range got.Agents {
				if got.Agents[i] != tc.want.Agents[i] {
					t.Errorf("Agents[%d]: got %q want %q", i, got.Agents[i], tc.want.Agents[i])
				}
			}
		})
	}
}

func TestEnsureAgentDoesNotMutateInput(t *testing.T) {
	// Slices are shared between caller and ensureAgent; appending to the
	// returned slice must not bleed into the original.
	original := protocol.Audience{Agents: []string{"alice"}}
	got := ensureAgent(original, "bob")
	if len(original.Agents) != 1 || original.Agents[0] != "alice" {
		t.Errorf("input mutated: %v", original.Agents)
	}
	if len(got.Agents) != 2 {
		t.Errorf("output mismatch: %v", got.Agents)
	}
}

func TestPublishAssignsIDAndTimestamp(t *testing.T) {
	persist := &mockPersister{}
	h := newHub(persist, nil)
	before := time.Now().UTC()
	got, err := h.Publish(protocol.Post{
		Author:   "alice",
		Audience: protocol.Audience{All: true},
		Content:  "hello",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.ID == "" {
		t.Error("ID not assigned")
	}
	if got.Timestamp.Before(before) {
		t.Errorf("Timestamp %s predates Publish call %s", got.Timestamp, before)
	}
	if persist.count() != 1 {
		t.Errorf("persister saw %d saves, want 1", persist.count())
	}
}

func TestPublishPersistsBeforeAppend(t *testing.T) {
	// A failing persister must leave both layers consistent: nothing
	// visible in memory, nothing emitted to subscribers.
	persist := &mockPersister{fail: errors.New("disk full")}
	h := newHub(persist, nil)
	_, err := h.Publish(protocol.Post{
		Author:   "alice",
		Audience: protocol.Audience{All: true},
		Content:  "should be lost",
	})
	if err == nil {
		t.Fatal("Publish with failing persister returned nil error")
	}
	if got := h.Visible("alice", time.Time{}); len(got) != 0 {
		t.Errorf("hub has %d visible posts after failed publish, want 0", len(got))
	}
}

func TestNewHubReplaysInitial(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	initial := []protocol.Post{
		{ID: "p1", Author: "alice", Audience: protocol.Audience{All: true}, Content: "broadcast", Timestamp: t0},
		{ID: "p2", Author: "alice", Audience: protocol.Audience{Agents: []string{"alice", "bob"}}, Content: "dm", Timestamp: t0.Add(time.Second)},
	}
	h := newHub(&mockPersister{}, initial)

	if _, ok := h.GetPost("p1"); !ok {
		t.Error("GetPost(p1) lost across replay")
	}
	if _, ok := h.GetPost("p2"); !ok {
		t.Error("GetPost(p2) lost across replay")
	}

	bobs := h.Visible("bob", time.Time{})
	if len(bobs) != 2 {
		t.Errorf("bob visible = %d, want 2", len(bobs))
	}

	carol := h.Visible("carol", time.Time{})
	if len(carol) != 1 || carol[0].ID != "p1" {
		t.Errorf("carol visible = %+v, want [p1]", carol)
	}
}

func TestVisibleSinceStrictAfter(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	initial := []protocol.Post{
		{ID: "a", Author: "alice", Audience: protocol.Audience{All: true}, Content: "first", Timestamp: t0},
		{ID: "b", Author: "alice", Audience: protocol.Audience{All: true}, Content: "second", Timestamp: t0.Add(time.Second)},
	}
	h := newHub(&mockPersister{}, initial)

	// since == a.Timestamp must exclude a (strictly after).
	got := h.Visible("anyone", t0)
	if len(got) != 1 || got[0].ID != "b" {
		t.Errorf("strict-after returned %+v, want [b]", got)
	}

	// since == zero returns everything.
	got = h.Visible("anyone", time.Time{})
	if len(got) != 2 {
		t.Errorf("zero since returned %d, want 2", len(got))
	}
}

func TestThread(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	dm := protocol.Audience{Agents: []string{"alice", "bob"}}
	initial := []protocol.Post{
		{ID: "p1", Author: "alice", Audience: dm, Content: "topic", Timestamp: t0},
		{ID: "r1", ParentID: "p1", Author: "bob", Audience: dm, Content: "reply 1", Timestamp: t0.Add(time.Second)},
		{ID: "r2", ParentID: "p1", Author: "alice", Audience: dm, Content: "reply 2", Timestamp: t0.Add(2 * time.Second)},
		{ID: "p2", Author: "carol", Audience: protocol.Audience{All: true}, Content: "elsewhere", Timestamp: t0.Add(3 * time.Second)},
	}
	h := newHub(&mockPersister{}, initial)

	post, replies, ok := h.Thread("p1", "alice")
	if !ok {
		t.Fatal("Thread(p1, alice) returned !ok")
	}
	if post.ID != "p1" {
		t.Errorf("Thread post.ID = %q want p1", post.ID)
	}
	if len(replies) != 2 {
		t.Errorf("Thread replies = %d, want 2", len(replies))
	}

	if _, _, ok := h.Thread("p1", "carol"); ok {
		t.Error("Thread(p1, carol) should be hidden by audience")
	}

	if _, _, ok := h.Thread("does-not-exist", "alice"); ok {
		t.Error("Thread(missing, alice) should return !ok")
	}
}

func TestSubscribeSnapshotIncludesPriorPosts(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	initial := []protocol.Post{
		{ID: "p1", Author: "alice", Audience: protocol.Audience{All: true}, Content: "before", Timestamp: t0},
		{ID: "p2", Author: "alice", Audience: protocol.Audience{Agents: []string{"bob"}}, Content: "for-bob", Timestamp: t0.Add(time.Second)},
	}
	h := newHub(&mockPersister{}, initial)

	snap, _, cancel := h.Subscribe("alice", time.Time{})
	defer cancel()
	if len(snap) != 1 || snap[0].Post.ID != "p1" {
		t.Errorf("alice snapshot = %+v, want only p1", snap)
	}
}

func TestSubscribeReceivesEventsAfterRegistration(t *testing.T) {
	h := newHub(&mockPersister{}, nil)
	_, events, cancel := h.Subscribe("alice", time.Time{})
	defer cancel()

	if _, err := h.Publish(protocol.Post{
		Author:   "bob",
		Audience: protocol.Audience{All: true},
		Content:  "live",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case evt := <-events:
		if evt.Post.Content != "live" {
			t.Errorf("got content %q, want %q", evt.Post.Content, "live")
		}
		if evt.Type != "post" {
			t.Errorf("got type %q, want post", evt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSubscribeRepliesGetReplyType(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	initial := []protocol.Post{
		{ID: "p1", Author: "alice", Audience: protocol.Audience{All: true}, Content: "topic", Timestamp: t0},
	}
	h := newHub(&mockPersister{}, initial)
	_, events, cancel := h.Subscribe("alice", t0)
	defer cancel()

	if _, err := h.Publish(protocol.Post{
		ParentID: "p1",
		Author:   "bob",
		Audience: protocol.Audience{All: true},
		Content:  "answer",
	}); err != nil {
		t.Fatalf("Publish reply: %v", err)
	}

	select {
	case evt := <-events:
		if evt.Type != "reply" {
			t.Errorf("event type = %q, want reply", evt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reply event")
	}
}

func TestHandlePostTitleRules(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "top-level post requires a title",
			body:       map[string]any{"audience": map[string]any{"all": true}, "content": "body without headline"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "top-level post with blank title is rejected",
			body:       map[string]any{"audience": map[string]any{"all": true}, "title": "   ", "content": "body"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "top-level post with title and empty content is accepted",
			body:       map[string]any{"audience": map[string]any{"all": true}, "title": "headline only"},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "top-level post with title and content is accepted",
			body:       map[string]any{"audience": map[string]any{"all": true}, "title": "headline", "content": "body"},
			wantStatus: http.StatusCreated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(&mockPersister{}, nil)
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			body, _ := json.Marshal(tc.body)
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/posts", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("X-Parley-Agent", "alice")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d (body=%q)", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

func TestHandlePostReplyRejectsTitle(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	srv := New(&mockPersister{}, []protocol.Post{
		{ID: "p1", Author: "alice", Audience: protocol.Audience{All: true}, Title: "topic", Timestamp: t0},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"parent_id": "p1",
		"title":     "should not be allowed",
		"content":   "reply body",
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/posts", bytes.NewReader(body))
	req.Header.Set("X-Parley-Agent", "bob")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400 (body=%q)", resp.StatusCode, b)
	}
}

func TestPublishSubscribeNoLossNoDuplicate(t *testing.T) {
	// The load-bearing invariant from architecture.md: every publish lands
	// either in the new subscriber's snapshot or on its channel, exactly
	// once. Race subscribe against in-flight publishers and verify the
	// total reconciles.
	h := newHub(&mockPersister{}, nil)
	const workers = 4
	const perWorker = 10 // 40 total — well under the 64-event channel buffer
	expected := workers * perWorker

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for k := 0; k < perWorker; k++ {
				_, err := h.Publish(protocol.Post{
					Author:   "alice",
					Audience: protocol.Audience{All: true},
					Content:  fmt.Sprintf("w%d-%d", workerID, k),
				})
				if err != nil {
					t.Errorf("Publish: %v", err)
				}
			}
		}(i)
	}

	// Give some publishers a head start so part of the result has to
	// come from the snapshot and the rest from the channel.
	time.Sleep(2 * time.Millisecond)
	snap, events, cancel := h.Subscribe("alice", time.Time{})
	defer cancel()

	seen := make(map[string]int)
	for _, evt := range snap {
		seen[evt.Post.ID]++
	}

	// Drain concurrently so the channel buffer never blocks a publisher.
	done := make(chan struct{})
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		for {
			select {
			case evt := <-events:
				seen[evt.Post.ID]++
			case <-done:
				for {
					select {
					case evt := <-events:
						seen[evt.Post.ID]++
					default:
						return
					}
				}
			}
		}
	}()

	wg.Wait()
	// Let the last fan-out sends land before we stop draining.
	time.Sleep(20 * time.Millisecond)
	close(done)
	drainWG.Wait()

	total := 0
	for id, n := range seen {
		total += n
		if n > 1 {
			t.Errorf("event %s seen %d times (duplicated across snapshot+channel)", id, n)
		}
	}
	if total != expected {
		t.Errorf("got %d events, want %d (snapshot=%d, channel=%d total)", total, expected, len(snap), total-len(snap))
	}
}
