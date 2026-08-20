// local_events.go 提供本地模式下的事件发布与订阅。
package agent

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	LocalEventExecutionStarted     = "execution_session.started"
	LocalEventExecutionCompleted   = "execution_session.completed"
	LocalEventExecutionInterrupted = "execution_session.interrupted"
	LocalEventExecutionFailed      = "execution_session.failed"
	LocalEventToolStatusChanged    = "tool.status_changed"
	LocalEventSnapshotUpdated      = "snapshot.updated"
	LocalEventOutputLine           = "execution_session.output_line"
	LocalEventIntervening          = "execution_session.intervening"
)

type LocalEvent struct {
	InstanceID string        `json:"instance_id"`
	EventID    string        `json:"event_id"`
	Type       string        `json:"type"`
	Timestamp  time.Time     `json:"timestamp"`
	Snapshot   LocalSnapshot `json:"snapshot"`
}

type LocalEventHub struct {
	mu          sync.RWMutex
	nextID      atomic.Uint64
	subscribers map[uint64]chan LocalEvent
}

func NewLocalEventHub() *LocalEventHub {
	return &LocalEventHub{
		subscribers: make(map[uint64]chan LocalEvent),
	}
}

func (h *LocalEventHub) Subscribe() (<-chan LocalEvent, func()) {
	id := h.nextID.Add(1)
	ch := make(chan LocalEvent, 16)

	h.mu.Lock()
	h.subscribers[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if subscriber, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(subscriber)
		}
		h.mu.Unlock()
	}
}

func (h *LocalEventHub) Publish(event LocalEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.EventID == "" {
		event.EventID = fmt.Sprintf("local-%d-%d", event.Timestamp.UnixMilli(), h.nextID.Add(1))
	}
	if event.InstanceID == "" {
		event.InstanceID = event.Snapshot.InstanceID
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, subscriber := range h.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func (h *LocalEventHub) PublishSnapshot(eventType string, snapshot LocalSnapshot) {
	h.Publish(LocalEvent{
		Type:     eventType,
		Snapshot: snapshot,
	})
}
