// Package client is the HTTP/SSE client used by the parley CLI.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yalochat/parley/internal/protocol"
)

type Client struct {
	BaseURL string
	Agent   string
	HTTP    *http.Client

	// ReconnectInitialDelay is the first wait after a dropped SSE stream.
	// Subsequent waits double up to ReconnectMaxDelay. Zero means default.
	ReconnectInitialDelay time.Duration
	// ReconnectMaxDelay caps the backoff between reconnect attempts. Zero
	// means default.
	ReconnectMaxDelay time.Duration
	// ReconnectMaxAttempts is the number of consecutive reconnects allowed
	// without delivering any events before Listen gives up. A successful
	// event resets the counter. Zero means default; negative disables the
	// cap (retry forever).
	ReconnectMaxAttempts int
}

func New(baseURL, agent string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Agent:   agent,
		HTTP:    &http.Client{},
	}
}

type PostInput struct {
	Audience protocol.Audience `json:"audience,omitempty"`
	Content  string            `json:"content"`
	ParentID string            `json:"parent_id,omitempty"`
}

// Post sends a new post (or reply, when ParentID is set) and returns the
// stored Post echoed back by the server.
func (c *Client) Post(ctx context.Context, in PostInput) (protocol.Post, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return protocol.Post{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/posts", bytes.NewReader(body))
	if err != nil {
		return protocol.Post{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Parley-Agent", c.Agent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return protocol.Post{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return protocol.Post{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var p protocol.Post
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return protocol.Post{}, err
	}
	return p, nil
}

// List returns posts visible to this agent. When since is non-zero, only
// posts with Timestamp >= since are returned.
func (c *Client) List(ctx context.Context, since time.Time) ([]protocol.Post, error) {
	u := c.BaseURL + "/posts"
	if !since.IsZero() {
		u += "?since=" + url.QueryEscape(since.Format(time.RFC3339Nano))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Parley-Agent", c.Agent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var posts []protocol.Post
	if err := json.NewDecoder(resp.Body).Decode(&posts); err != nil {
		return nil, err
	}
	return posts, nil
}

// View fetches a single post by id along with its direct replies.
func (c *Client) View(ctx context.Context, id string) (protocol.Thread, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/posts/"+url.PathEscape(id), nil)
	if err != nil {
		return protocol.Thread{}, err
	}
	req.Header.Set("X-Parley-Agent", c.Agent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return protocol.Thread{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return protocol.Thread{}, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return protocol.Thread{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var t protocol.Thread
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return protocol.Thread{}, err
	}
	return t, nil
}

// ErrNotFound is returned by View when the post id is unknown or hidden.
var ErrNotFound = errors.New("post not found")

// Listen opens an SSE stream and invokes onEvent for each parsed event.
// When since is non-zero, the snapshot the server replays on connect is
// limited to events with Timestamp strictly after since — useful for
// resuming from a cursor without re-reading old history.
//
// Listen reconnects automatically when the stream drops (server restart,
// network blip, clean EOF, 5xx) with exponential backoff. Each reconnect
// uses the timestamp of the most recent event delivered so no events are
// re-played and none are missed. Reconnect attempts that deliver no
// events accumulate; after ReconnectMaxAttempts consecutive empty
// attempts Listen returns the last underlying error. A successful event
// resets the counter.
//
// Returns when the context is cancelled (surfaced as context.Canceled),
// the callback returns an error, or a non-retryable condition occurs
// (4xx response, malformed SSE payload, retry budget exhausted).
func (c *Client) Listen(ctx context.Context, since time.Time, onEvent func(protocol.Event) error) error {
	initialDelay := c.ReconnectInitialDelay
	if initialDelay <= 0 {
		initialDelay = 500 * time.Millisecond
	}
	maxDelay := c.ReconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	maxAttempts := c.ReconnectMaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 10
	}

	latest := since
	failures := 0
	delay := initialDelay
	var lastErr error

	for {
		gotEvent, retryable, err := c.streamEvents(ctx, latest, func(evt protocol.Event) error {
			if err := onEvent(evt); err != nil {
				return err
			}
			if evt.Post.Timestamp.After(latest) {
				latest = evt.Post.Timestamp
			}
			return nil
		})
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil && !retryable {
			return err
		}
		if err != nil {
			lastErr = err
		}
		if gotEvent {
			failures = 0
			delay = initialDelay
			lastErr = nil
		} else {
			failures++
			if maxAttempts > 0 && failures >= maxAttempts {
				if lastErr == nil {
					lastErr = fmt.Errorf("listen: %d reconnects without events", failures)
				}
				return lastErr
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// streamEvents opens one SSE connection and processes it to completion.
// It returns:
//   - gotEvent:   at least one event was successfully delivered to onEvent.
//   - retryable:  the caller should reconnect after a backoff delay.
//   - err:        nil on clean EOF, non-nil on any other termination.
//
// Categorisation:
//   - HTTP dial / read errors → retryable.
//   - HTTP 5xx                → retryable.
//   - HTTP 4xx                → fatal.
//   - JSON decode failure     → fatal (malformed stream is a bug).
//   - onEvent error           → fatal (caller wants out).
//   - Clean EOF               → retryable (server closed cleanly).
func (c *Client) streamEvents(ctx context.Context, since time.Time, onEvent func(protocol.Event) error) (gotEvent bool, retryable bool, err error) {
	u := c.BaseURL + "/events"
	if !since.IsZero() {
		u += "?since=" + url.QueryEscape(since.Format(time.RFC3339Nano))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Parley-Agent", c.Agent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		e := fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		retry := resp.StatusCode >= 500 && resp.StatusCode < 600
		return false, retry, e
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var evt protocol.Event
			if err := json.Unmarshal([]byte(data.String()), &evt); err != nil {
				return gotEvent, false, fmt.Errorf("decode SSE payload: %w", err)
			}
			data.Reset()
			if err := onEvent(evt); err != nil {
				return gotEvent, false, err
			}
			gotEvent = true
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			rest = strings.TrimPrefix(rest, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(rest)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return gotEvent, true, err
	}
	return gotEvent, true, nil
}
