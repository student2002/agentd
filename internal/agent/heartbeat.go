// heartbeat.go manages the periodic heartbeat sending of the Agent Daemon.
//
// This file maintains the online status of the agent runtime, mainly including:
//   - Heartbeat struct: encapsulates the configuration and lifecycle management
//     of the heartbeat loop
//   - Start / Stop: start and stop the heartbeat goroutine, supporting graceful
//     shutdown
//   - heartbeat interval: 30 seconds by default, configurable
//
// The heartbeat is sent via HTTP POST to the Server endpoint
// /api/workspaces/{id}/runtimes/{id}/heartbeat.
// On heartbeat failure only a log is recorded; the daemon run is not
// interrupted.
package agent

import (
	"context"
	"log"
	"sync"
	"time"
)

// Heartbeat manages periodic heartbeat sending, maintaining the online status of
// the agent runtime.
type Heartbeat struct {
	client      *Client
	workspaceID string
	runtimeID   string
	interval    time.Duration
	callbacks   HeartbeatCallbacks
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

type HeartbeatCallbacks struct {
	OnSuccess func(time.Time)
	OnError   func(error)
}

// NewHeartbeat creates a new heartbeat manager.
func NewHeartbeat(client *Client, workspaceID, runtimeID string, interval time.Duration) *Heartbeat {
	return NewHeartbeatWithCallbacks(client, workspaceID, runtimeID, interval, HeartbeatCallbacks{})
}

func NewHeartbeatWithCallbacks(client *Client, workspaceID, runtimeID string, interval time.Duration, callbacks HeartbeatCallbacks) *Heartbeat {
	return &Heartbeat{
		client:      client,
		workspaceID: workspaceID,
		runtimeID:   runtimeID,
		interval:    interval,
		callbacks:   callbacks,
		stopCh:      make(chan struct{}),
	}
}

// Start starts the heartbeat loop.
func (h *Heartbeat) Start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := h.client.Heartbeat(context.Background(), h.workspaceID, h.runtimeID); err != nil {
					log.Printf("[heartbeat] failed: %v", err)
					if h.callbacks.OnError != nil {
						h.callbacks.OnError(err)
					}
					continue
				}
				if h.callbacks.OnSuccess != nil {
					h.callbacks.OnSuccess(time.Now().UTC())
				}
			case <-h.stopCh:
				return
			}
		}
	}()
}

// Stop stops the heartbeat loop and waits for the goroutine to exit.
func (h *Heartbeat) Stop() {
	close(h.stopCh)
	h.wg.Wait()
}
