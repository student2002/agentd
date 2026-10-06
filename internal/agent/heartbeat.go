// heartbeat.go manages the daemon-level periodic heartbeat.
//
// The heartbeat keeps the daemon online and reports per-instance busy status
// (an instance is busy while its executor is running). The response carries
// the desired-agents set, handed to the reconcile loop, and the server's
// heartbeat interval. A 404 response means the daemon row was deleted
// server-side: the heartbeat stops itself and surfaces the error — the
// operator must re-provision the daemon (web UI) and update the local token.
package agent

import (
	"context"
	"log"
	"sync"
	"time"
)

// defaultHeartbeatInterval is the heartbeat interval used until the server
// reports its own preference.
const defaultHeartbeatInterval = 30 * time.Second

// DaemonHeartbeat manages the periodic daemon heartbeat.
type DaemonHeartbeat struct {
	client   *Client
	interval time.Duration
	busyFn   func() []AgentBusy

	// onResponse receives every successful response (desired agents).
	onResponse func(*DaemonHeartbeatResponse)

	callbacks HeartbeatCallbacks

	mu      sync.Mutex
	stopped bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// HeartbeatCallbacks carries optional success/error notifications used to
// mirror heartbeat state into the local control snapshot.
type HeartbeatCallbacks struct {
	OnSuccess func(time.Time)
	OnError   func(error)
}

// NewDaemonHeartbeat creates a new daemon heartbeat manager. busyFn returns
// the per-instance busy statuses sent with every beat; onResponse receives
// each successful response.
func NewDaemonHeartbeat(client *Client, interval time.Duration, busyFn func() []AgentBusy, onResponse func(*DaemonHeartbeatResponse), callbacks HeartbeatCallbacks) *DaemonHeartbeat {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	return &DaemonHeartbeat{
		client:     client,
		interval:   interval,
		busyFn:     busyFn,
		onResponse: onResponse,
		callbacks:  callbacks,
		stopCh:     make(chan struct{}),
	}
}

// SetInterval replaces the heartbeat interval (used when the server reports
// its own preference in the heartbeat response).
func (h *DaemonHeartbeat) SetInterval(interval time.Duration) {
	if interval <= 0 {
		return
	}
	h.mu.Lock()
	h.interval = interval
	h.mu.Unlock()
}

// Start starts the heartbeat loop.
func (h *DaemonHeartbeat) Start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		var ticker *time.Ticker
		var tickerCh <-chan time.Time
		setTicker := func() {
			h.mu.Lock()
			interval := h.interval
			h.mu.Unlock()
			if ticker != nil {
				ticker.Stop()
			}
			ticker = time.NewTicker(interval)
			tickerCh = ticker.C
		}
		setTicker()
		defer func() {
			if ticker != nil {
				ticker.Stop()
			}
		}()

		for {
			select {
			case <-tickerCh:
				if h.beat() {
					// 404: daemon deleted server-side — stop the loop.
					return
				}
				setTicker()
			case <-h.stopCh:
				return
			}
		}
	}()
}

// beat sends one heartbeat. It returns true when the daemon was rejected
// terminally (404 deleted / 401 token revoked) and the loop must stop.
func (h *DaemonHeartbeat) beat() bool {
	var agents []AgentBusy
	if h.busyFn != nil {
		agents = h.busyFn()
	}
	resp, err := h.client.DaemonHeartbeat(context.Background(), agents)
	if err != nil {
		log.Printf("[heartbeat] failed: %v", err)
		if h.callbacks.OnError != nil {
			h.callbacks.OnError(err)
		}
		if IsNotFound(err) || IsUnauthorized(err) {
			log.Printf("[heartbeat] daemon rejected by server (deleted or token revoked) — re-provision the daemon from that workspace's web UI and re-add the connection with the new token")
			return true
		}
		return false
	}
	if h.callbacks.OnSuccess != nil {
		h.callbacks.OnSuccess(time.Now().UTC())
	}
	if resp != nil {
		if resp.HeartbeatInterval > 0 {
			h.SetInterval(time.Duration(resp.HeartbeatInterval) * time.Second)
		}
		if h.onResponse != nil {
			h.onResponse(resp)
		}
	}
	return false
}

// Stop stops the heartbeat loop and waits for the goroutine to exit.
func (h *DaemonHeartbeat) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	h.mu.Unlock()
	close(h.stopCh)
	h.wg.Wait()
}
