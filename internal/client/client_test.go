package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yalochat/parley/internal/protocol"
)

// sseHandler is a controllable SSE endpoint. Each connection pops one
// scripted "round" from rounds; if the script is exhausted the handler
// reports a fatal test failure (Listen made more requests than expected).
//
// A round is the per-connection script:
//   - status: HTTP status to write. 200 streams events; non-200 writes body.
//   - events: events to write before closing (only used when status=200).
//   - hold:   if true, wait for the test to signal close via the round's
//     done channel before returning (useful for ctx-cancellation tests).
//   - body:   raw body to write for non-200 responses.
type round struct {
	status int
	events []protocol.Event
	body   string
	hold   bool
	done   chan struct{}
}

type sseHandler struct {
	t        *testing.T
	mu       sync.Mutex
	rounds   []*round
	calls    int32
	queries  []string // captured raw query strings, in order
	agent    string   // captured X-Parley-Agent of latest call
	failOnce sync.Once
}

func newSSEHandler(t *testing.T, rounds ...*round) *sseHandler {
	return &sseHandler{t: t, rounds: rounds}
}

func (h *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	idx := int(atomic.AddInt32(&h.calls, 1)) - 1
	h.mu.Lock()
	h.queries = append(h.queries, r.URL.RawQuery)
	h.agent = r.Header.Get("X-Parley-Agent")
	if idx >= len(h.rounds) {
		h.mu.Unlock()
		h.failOnce.Do(func() {
			h.t.Errorf("sseHandler: unexpected request #%d (only %d rounds scripted)", idx+1, len(h.rounds))
		})
		http.Error(w, "no more rounds", http.StatusInternalServerError)
		return
	}
	rd := h.rounds[idx]
	h.mu.Unlock()

	if rd.status != 0 && rd.status != http.StatusOK {
		w.WriteHeader(rd.status)
		_, _ = w.Write([]byte(rd.body))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, evt := range rd.events {
		payload, err := evt.AsJSON()
		if err != nil {
			h.t.Errorf("encode event: %v", err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if rd.hold {
		select {
		case <-rd.done:
		case <-r.Context().Done():
		}
	}
}

func (h *sseHandler) callCount() int      { return int(atomic.LoadInt32(&h.calls)) }
func (h *sseHandler) capturedQueries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.queries))
	copy(out, h.queries)
	return out
}

func makeEvent(id, author, content string, ts time.Time) protocol.Event {
	aud, _ := protocol.ParseAudience("all")
	return protocol.Event{
		Type: "post",
		Post: protocol.Post{
			ID:        id,
			Author:    author,
			Audience:  aud,
			Content:   content,
			Timestamp: ts,
		},
	}
}

func fastClient(baseURL string) *Client {
	c := New(baseURL, "alice")
	c.ReconnectInitialDelay = time.Millisecond
	c.ReconnectMaxDelay = 5 * time.Millisecond
	c.ReconnectMaxAttempts = 5
	return c
}

func TestListen_ReconnectsAfterEOFAndAdvancesSince(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)
	h := newSSEHandler(t,
		&round{status: 200, events: []protocol.Event{makeEvent("a", "bob", "first", t1)}},
		&round{status: 200, events: []protocol.Event{makeEvent("b", "bob", "second", t2)}},
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := fastClient(srv.URL)
	var got []protocol.Event
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stop := errors.New("stop")
	err := c.Listen(ctx, time.Time{}, func(evt protocol.Event) error {
		got = append(got, evt)
		if len(got) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Listen err: got %v, want %v", err, stop)
	}
	if len(got) != 2 || got[0].Post.ID != "a" || got[1].Post.ID != "b" {
		t.Fatalf("events: got %+v", got)
	}
	queries := h.capturedQueries()
	if len(queries) != 2 {
		t.Fatalf("connection count: got %d (%v), want 2", len(queries), queries)
	}
	if queries[0] != "" {
		t.Errorf("first request: got query %q, want empty (no since)", queries[0])
	}
	if !strings.Contains(queries[1], "since=") {
		t.Errorf("second request: got query %q, want since= param", queries[1])
	}
	// The since query should be exactly t1 (the latest event from round 1),
	// so the server skips re-replaying it.
	wantSince := t1.Format(time.RFC3339Nano)
	if !strings.Contains(queries[1], wantSince) {
		// URL-encoded ':' is %3A — check both forms.
		if !strings.Contains(queries[1], strings.ReplaceAll(wantSince, ":", "%3A")) {
			t.Errorf("second request: got %q, want since=%s", queries[1], wantSince)
		}
	}
}

func TestListen_FatalOn4xx(t *testing.T) {
	h := newSSEHandler(t, &round{status: 400, body: "bad request"})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := fastClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := c.Listen(ctx, time.Time{}, func(protocol.Event) error { return nil })
	if err == nil {
		t.Fatalf("Listen: got nil error, want failure")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err: got %v, want it to mention 400", err)
	}
	if h.callCount() != 1 {
		t.Errorf("calls: got %d, want 1 (4xx must not be retried)", h.callCount())
	}
}

func TestListen_Retries5xxThenSucceeds(t *testing.T) {
	t1 := time.Unix(1700000001, 0).UTC()
	h := newSSEHandler(t,
		&round{status: 503, body: "unavailable"},
		&round{status: 200, events: []protocol.Event{makeEvent("a", "bob", "hi", t1)}},
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := fastClient(srv.URL)
	stop := errors.New("stop")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var got []protocol.Event
	err := c.Listen(ctx, time.Time{}, func(evt protocol.Event) error {
		got = append(got, evt)
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Listen err: got %v, want %v", err, stop)
	}
	if len(got) != 1 || got[0].Post.ID != "a" {
		t.Fatalf("got events: %+v", got)
	}
	if h.callCount() != 2 {
		t.Errorf("calls: got %d, want 2 (one 503 retry + success)", h.callCount())
	}
}

func TestListen_GivesUpAfterMaxConsecutiveFailures(t *testing.T) {
	// Six rounds: every one a 502 (retryable). Cap = 5, so we expect Listen
	// to abort after 5 attempts.
	rounds := make([]*round, 6)
	for i := range rounds {
		rounds[i] = &round{status: 502, body: "bad gateway"}
	}
	h := newSSEHandler(t, rounds...)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := fastClient(srv.URL)
	c.ReconnectMaxAttempts = 5
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Listen(ctx, time.Time{}, func(protocol.Event) error { return nil })
	if err == nil {
		t.Fatalf("Listen: got nil error, want failure after retry budget")
	}
	if h.callCount() != 5 {
		t.Errorf("calls: got %d, want 5 (cap=5)", h.callCount())
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err: got %v, want it to mention 502 (last underlying error)", err)
	}
}

func TestListen_ContextCancellationDuringBackoff(t *testing.T) {
	// First round returns EOF immediately (no events). Listen will sleep
	// before the next attempt — we cancel during that sleep and expect a
	// context.Canceled (or DeadlineExceeded) return.
	h := newSSEHandler(t,
		&round{status: 200, events: nil},
		&round{status: 200, events: nil},
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := New(srv.URL, "alice")
	c.ReconnectInitialDelay = 200 * time.Millisecond
	c.ReconnectMaxDelay = 200 * time.Millisecond
	c.ReconnectMaxAttempts = 10

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Listen(ctx, time.Time{}, func(protocol.Event) error { return nil })
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen err: got %v, want ctx error", err)
	}
	// Should exit during the first backoff sleep (~50ms), well before the
	// full 200ms.
	if elapsed > 150*time.Millisecond {
		t.Errorf("Listen took %v, expected exit during backoff (~50ms)", elapsed)
	}
}

func TestListen_OnEventErrorIsFatal(t *testing.T) {
	t1 := time.Unix(1700000010, 0).UTC()
	h := newSSEHandler(t,
		&round{status: 200, events: []protocol.Event{
			makeEvent("a", "bob", "x", t1),
			makeEvent("b", "bob", "y", t1.Add(time.Second)),
		}},
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := fastClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	want := errors.New("caller bail")
	err := c.Listen(ctx, time.Time{}, func(protocol.Event) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Listen err: got %v, want %v", err, want)
	}
	if h.callCount() != 1 {
		t.Errorf("calls: got %d, want 1 (callback error must not retry)", h.callCount())
	}
}
