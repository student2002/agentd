// sse_client.go implements the SSE (Server-Sent Events) client, used to receive
// real-time event pushes from the Server.
//
// This file provides the long-lived event channel between the Agent Daemon and
// the Server, mainly including:
//   - SSEClient struct: encapsulates SSE connection management, supporting
//     reconnection and exponential backoff
//   - Start / Stop: start and stop the SSE connection, gracefully canceling
//     in-flight HTTP requests
//   - run: the main loop, managing the connection lifecycle and automatic
//     reconnection
//   - connect: establishes the SSE connection, setting Last-Event-ID for event
//     replay
//   - readStream: parses the SSE event stream, extracting the id, event, and
//     data fields
//   - dispatchEvent: dispatches the parsed event to the registered callback
//
// Supported SSE event types: node:pending, node:continuation_invite,
// task:interrupt, etc.
// On reconnection, event replay compensation is achieved via the Last-Event-ID
// header.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSEClient connects to the server's SSE endpoint to receive real-time event
// pushes, supporting reconnection and event replay.
type SSEClient struct {
	serverURL   string
	workspaceID string
	runtimeID   string
	tokenFn     func() string // callback returning the latest auth token
	onEvent     func(eventType string, data json.RawMessage)
	callbacks   SSECallbacks
	stopCh      chan struct{}
	wg          sync.WaitGroup

	mu             sync.Mutex
	connected      bool
	lastEventID    string
	backoffSeconds int

	cancelFn context.CancelFunc // cancels the context of the current SSE HTTP request
	cancelMu sync.Mutex
}

type SSECallbacks struct {
	OnConnected    func(lastEventID string)
	OnDisconnected func(error)
}

// NewSSEClient creates a new SSE client.
// tokenFn is a callback returning the current best auth token (session token or
// API token).
func NewSSEClient(serverURL, workspaceID, runtimeID string, tokenFn func() string, onEvent func(string, json.RawMessage)) *SSEClient {
	return NewSSEClientWithCallbacks(serverURL, workspaceID, runtimeID, tokenFn, onEvent, SSECallbacks{})
}

func NewSSEClientWithCallbacks(serverURL, workspaceID, runtimeID string, tokenFn func() string, onEvent func(string, json.RawMessage), callbacks SSECallbacks) *SSEClient {
	return &SSEClient{
		serverURL:      serverURL,
		workspaceID:    workspaceID,
		runtimeID:      runtimeID,
		tokenFn:        tokenFn,
		onEvent:        onEvent,
		callbacks:      callbacks,
		stopCh:         make(chan struct{}),
		backoffSeconds: 1,
	}
}

// Start connects to the SSE endpoint and begins receiving events.
func (c *SSEClient) Start() error {
	c.wg.Add(1)
	go c.run()
	return nil
}

// Stop disconnects the SSE client and cancels in-flight HTTP requests.
func (c *SSEClient) Stop() {
	close(c.stopCh)
	// Cancel all in-flight HTTP requests so that readStream unblocks
	c.cancelMu.Lock()
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.cancelMu.Unlock()
	c.wg.Wait()
}

// IsConnected returns whether the SSE client is currently connected.
func (c *SSEClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// setConnected sets the connection state flag of the SSE client.
//
// Parameters:
//   - v: the connection state; true means connected, false means disconnected
func (c *SSEClient) setConnected(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = v
}

// run is the main loop goroutine of the SSE client, managing the connection
// lifecycle and automatic reconnection.
// On connection failure it retries with an exponential backoff strategy, with a
// maximum backoff interval of 30 seconds.
// It exits gracefully on receiving a stop signal.
func (c *SSEClient) run() {
	defer c.wg.Done()

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		err := c.connect()
		if err != nil {
			log.Printf("[sse] connection error: %v", err)
		}

		c.setConnected(false)
		if c.callbacks.OnDisconnected != nil {
			c.callbacks.OnDisconnected(err)
		}

		// Check whether it should stop before reconnecting
		select {
		case <-c.stopCh:
			return
		default:
		}

		// Exponential backoff
		wait := time.Duration(c.backoffSeconds) * time.Second
		log.Printf("[sse] reconnecting in %v...", wait)

		select {
		case <-time.After(wait):
		case <-c.stopCh:
			return
		}

		c.mu.Lock()
		c.backoffSeconds *= 2
		if c.backoffSeconds > 30 {
			c.backoffSeconds = 30
		}
		c.mu.Unlock()
	}
}

// connect establishes an HTTP long-lived connection to the Server SSE endpoint.
// It sets the Last-Event-ID header for event replay compensation on
// reconnection.
// On a successful connection it resets the backoff counter and enters the event
// stream read loop.
//
// Returns:
//   - error: returned on connection failure or event stream read error
func (c *SSEClient) connect() error {
	url := fmt.Sprintf("%s/api/workspaces/%s/runtimes/%s/events", c.serverURL, c.workspaceID, c.runtimeID)

	// Create a cancellable context so that Stop() can abort the HTTP request
	ctx, cancel := context.WithCancel(context.Background())
	c.cancelMu.Lock()
	c.cancelFn = cancel
	c.cancelMu.Unlock()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-API-Key", c.tokenFn())

	// Send Last-Event-ID for event replay on reconnection
	c.mu.Lock()
	if c.lastEventID != "" {
		req.Header.Set("Last-Event-ID", c.lastEventID)
	}
	c.mu.Unlock()

	// Use a client with no timeout for the SSE long-lived connection
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	c.setConnected(true)
	if c.callbacks.OnConnected != nil {
		c.callbacks.OnConnected(c.LastEventID())
	}
	// Reset the backoff after a successful connection
	c.mu.Lock()
	c.backoffSeconds = 1
	c.mu.Unlock()

	log.Printf("[sse] connected to %s", url)

	return c.readStream(resp)
}

func (c *SSEClient) LastEventID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventID
}

// readStream parses the SSE event stream line by line, extracting the id, event,
// and data fields.
// It follows the SSE spec: blank lines separate events, lines starting with a
// colon are comments, and the first space after data: is automatically removed.
// After parsing it calls dispatchEvent to dispatch the event.
//
// Parameters:
//   - resp: the SSE HTTP response object
//
// Returns:
//   - error: returned when the scanner encounters a read error
func (c *SSEClient) readStream(resp *http.Response) error {
	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	var dataBuilder strings.Builder

	for scanner.Scan() {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		line := scanner.Text()

		if line == "" {
			// Blank line = end of event
			if dataBuilder.Len() > 0 {
				c.dispatchEvent(eventType, dataBuilder.String())
				dataBuilder.Reset()
				eventType = ""
			}
			continue
		}

		if strings.HasPrefix(line, "id:") {
			id := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			c.mu.Lock()
			c.lastEventID = id
			c.mu.Unlock()
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			dataLine := strings.TrimPrefix(line, "data:")
			// SSE spec: a single space after "data:" (if present) is removed
			if len(dataLine) > 0 && dataLine[0] == ' ' {
				dataLine = dataLine[1:]
			}
			if dataBuilder.Len() > 0 {
				dataBuilder.WriteString("\n")
			}
			dataBuilder.WriteString(dataLine)
			continue
		}

		// Lines starting with ':' are comments, ignored
	}

	return scanner.Err()
}

// dispatchEvent dispatches the parsed SSE event to the registered callback.
// If the event type is empty, it defaults to "message".
//
// Parameters:
//   - eventType: the event type (e.g. "node:pending", "task:interrupt")
//   - data: the raw JSON string of the event data
func (c *SSEClient) dispatchEvent(eventType, data string) {
	if eventType == "" {
		eventType = "message"
	}

	raw := json.RawMessage(data)
	if c.onEvent != nil {
		c.onEvent(eventType, raw)
	}
}
