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
// Returns when the context is cancelled (surfaced as context.Canceled) or
// when the stream ends.
func (c *Client) Listen(ctx context.Context, since time.Time, onEvent func(protocol.Event) error) error {
	u := c.BaseURL + "/events"
	if !since.IsZero() {
		u += "?since=" + url.QueryEscape(since.Format(time.RFC3339Nano))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Parley-Agent", c.Agent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
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
				return fmt.Errorf("decode SSE payload: %w", err)
			}
			data.Reset()
			if err := onEvent(evt); err != nil {
				return err
			}
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
		return err
	}
	return nil
}
