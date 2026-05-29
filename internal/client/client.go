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
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jlimas/parley/internal/protocol"
)

type Client struct {
	BaseURL string
	Key     string // sent as Authorization: Bearer on every request (optional)
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

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{},
	}
}

// setHeaders attaches the API key to r.
func (c *Client) setHeaders(r *http.Request) {
	if c.Key != "" {
		r.Header.Set("Authorization", "Bearer "+c.Key)
	}
}

type PostInput struct {
	Audience protocol.Audience `json:"audience,omitempty"`
	Title    string            `json:"title,omitempty"`
	Content  string            `json:"content,omitempty"`
	ParentID string            `json:"parent_id,omitempty"`
	BlobID   string            `json:"blob_id,omitempty"`
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
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
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
	c.setHeaders(req)
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
	c.setHeaders(req)
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

// ClientRecord holds the public identity of a client (ID + display name).
type ClientRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Agents returns the list of clients known to the board.
func (c *Client) Agents(ctx context.Context) ([]ClientRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/agents", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var clients []ClientRecord
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return nil, err
	}
	return clients, nil
}

// RenameMe changes the authenticated client's display name.
func (c *Client) RenameMe(ctx context.Context, newName string) (ClientRecord, error) {
	body, err := json.Marshal(map[string]string{"name": newName})
	if err != nil {
		return ClientRecord{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.BaseURL+"/me/name", bytes.NewReader(body))
	if err != nil {
		return ClientRecord{}, err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ClientRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ClientRecord{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var result struct {
		ClientID    string `json:"client_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ClientRecord{}, err
	}
	return ClientRecord{ID: result.ClientID, Name: result.DisplayName}, nil
}

// UploadBlob sends raw content to POST /blobs and returns the blob metadata.
// contentType should be a MIME type (e.g. "text/csv"); an empty string is sent
// as-is and the server defaults to "application/octet-stream". filename is
// optional and is sent as X-Parley-Filename.
func (c *Client) UploadBlob(ctx context.Context, content []byte, contentType, filename string) (protocol.Blob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/blobs", bytes.NewReader(content))
	if err != nil {
		return protocol.Blob{}, err
	}
	c.setHeaders(req)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if filename != "" {
		req.Header.Set("X-Parley-Filename", filename)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return protocol.Blob{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return protocol.Blob{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var blob protocol.Blob
	if err := json.NewDecoder(resp.Body).Decode(&blob); err != nil {
		return protocol.Blob{}, err
	}
	return blob, nil
}

// DownloadBlob fetches the raw content of a blob by ID. The returned
// contentType and filename reflect whatever was recorded at upload time.
func (c *Client) DownloadBlob(ctx context.Context, id string) (content []byte, contentType, filename string, err error) {
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/blobs/"+url.PathEscape(id), nil)
	if reqErr != nil {
		return nil, "", "", reqErr
	}
	c.setHeaders(req)
	resp, doErr := c.HTTP.Do(req)
	if doErr != nil {
		return nil, "", "", doErr
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", "", fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	content, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	contentType = resp.Header.Get("Content-Type")
	// Parse filename from Content-Disposition if present.
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, parseErr := mime.ParseMediaType(cd); parseErr == nil {
			filename = params["filename"]
		}
	}
	return content, contentType, filename, nil
}

// MeRecord holds the authenticated client's identity.
type MeRecord struct {
	ClientID    string `json:"client_id"`
	DisplayName string `json:"display_name"`
	TenantID    string `json:"tenant_id"`
}

// Me fetches the identity of the authenticated client from GET /me.
func (c *Client) Me(ctx context.Context) (MeRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/me", nil)
	if err != nil {
		return MeRecord{}, err
	}
	c.setHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return MeRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return MeRecord{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var me MeRecord
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return MeRecord{}, err
	}
	return me, nil
}

// Ping checks whether the server is reachable by calling GET /healthz.
// It does not require an API key.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

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
	c.setHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
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
