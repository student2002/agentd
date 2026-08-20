package agent

import (
	"sync"
	"sync/atomic"
	"time"
)

// LogLine is a single execution output line captured for local inspection.
type LogLine struct {
	Seq    uint64    `json:"seq"`
	TaskID int32     `json:"task_id"`
	NodeID string    `json:"node_id"`
	Line   string    `json:"line"`
	Ts     time.Time `json:"ts"`
}

// LogBuffer is a bounded ring buffer of execution output lines that also
// broadcasts new lines to live subscribers. It is used by agentd's local
// control API to expose recent + streaming logs to the Baozi desktop client.
//
// Data source/permission boundary: lines come from the executor's onOutput
// callback during task execution on this agentd only — never cross-workspace
// or cross-project. Failure degrades to empty (a nil buffer is a no-op).
type LogBuffer struct {
	capacity int

	mu          sync.Mutex
	ring        []LogLine
	head        int // index of next write
	full        bool
	subscribers map[uint64]chan LogLine

	nextSubID atomic.Uint64
	seq       atomic.Uint64
}

// NewLogBuffer creates a LogBuffer holding up to capacity lines. capacity is
// clamped to a minimum of 1.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &LogBuffer{
		capacity:    capacity,
		ring:        make([]LogLine, capacity),
		subscribers: make(map[uint64]chan LogLine),
	}
}

// Append stores a line and broadcasts it to all subscribers. If line.Ts is
// zero it is set to now. Seq is assigned monotonically.
func (b *LogBuffer) Append(line LogLine) {
	if b == nil {
		return
	}
	line.Seq = b.seq.Add(1)
	if line.Ts.IsZero() {
		line.Ts = time.Now()
	}

	b.mu.Lock()
	b.ring[b.head] = line
	b.head = (b.head + 1) % b.capacity
	if b.head == 0 {
		b.full = true
	}
	subs := make([]chan LogLine, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		// Non-blocking: a slow subscriber must not stall execution.
		select {
		case ch <- line:
		default:
		}
	}
}

// Recent returns up to n lines in chronological (oldest→newest) order.
func (b *LogBuffer) Recent(n int) []LogLine {
	if b == nil {
		return nil
	}
	stored := b.storedLocked()
	if n < 0 || n > len(stored) {
		n = len(stored)
	}
	out := make([]LogLine, n)
	copy(out, stored[len(stored)-n:])
	return out
}

// Since returns all lines with Seq strictly greater than seq, in chronological
// order.
func (b *LogBuffer) Since(seq uint64) []LogLine {
	if b == nil {
		return nil
	}
	stored := b.storedLocked()
	out := make([]LogLine, 0, len(stored))
	for _, l := range stored {
		if l.Seq > seq {
			out = append(out, l)
		}
	}
	return out
}

// Subscribe returns a channel receiving future appends and an unsubscribe
// function. The channel is buffered (64); overflowing lines are dropped.
func (b *LogBuffer) Subscribe() (<-chan LogLine, func()) {
	if b == nil {
		ch := make(chan LogLine)
		return ch, func() {}
	}
	id := b.nextSubID.Add(1)
	ch := make(chan LogLine, 64)

	b.mu.Lock()
	b.subscribers[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if ch, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// storedLocked returns all stored lines in chronological order. Caller must
// hold b.mu (or accept a racy snapshot — used here only under lock).
func (b *LogBuffer) storedLocked() []LogLine {
	if !b.full {
		// Not yet wrapped: ring[0:head] holds the live lines in order.
		out := make([]LogLine, b.head)
		copy(out, b.ring[:b.head])
		return out
	}
	// Wrapped: head points at the oldest entry.
	out := make([]LogLine, b.capacity)
	copy(out, b.ring[b.head:])
	copy(out[b.capacity-b.head:], b.ring[:b.head])
	return out
}
