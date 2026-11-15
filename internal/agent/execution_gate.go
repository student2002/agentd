// execution_gate.go implements the shared execution slot of one agent
// instance: node claims, continuation-invite auto-claims and invocation claims
// all pass through it, keeping the "one agent instance executes serially"
// semantics with a single authority instead of scattered IsRunning checks.
// Capacity is fixed at 1; opening parallelism later is an explicit,
// separately-tested decision. A terminal release wakes every waiter and fires
// the release hook so both consumers rescan without polling.
package agent

import (
	"context"
	"sync"
)

// ExecutionGate is a single-slot gate with broadcast wake-up: releasing it
// wakes every waiter at once and they re-contend, so both the node watcher
// and the invocation scanner observe slot availability without polling.
type ExecutionGate struct {
	mu          sync.Mutex
	held        bool
	notify      chan struct{} // closed on release and recreated; all waiters observe it
	releaseHook func()        // called outside the lock after every release
}

// NewExecutionGate creates an open (unheld) gate.
func NewExecutionGate() *ExecutionGate {
	return &ExecutionGate{notify: make(chan struct{})}
}

// SetReleaseHook installs the callback fired after every release (both
// consumers rescan: the invocation scanner and the node watcher). It must be
// non-blocking — it is called on the releasing goroutine.
func (g *ExecutionGate) SetReleaseHook(fn func()) {
	g.mu.Lock()
	g.releaseHook = fn
	g.mu.Unlock()
}

// fireReleaseHook runs the release hook outside the gate lock.
func (g *ExecutionGate) fireReleaseHook() {
	g.mu.Lock()
	hook := g.releaseHook
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// TryAcquire takes the slot without waiting. ok=false means it is busy and
// the returned release is a no-op.
func (g *ExecutionGate) TryAcquire() (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		return func() {}, false
	}
	g.held = true
	notify := g.notify
	return func() {
		g.mu.Lock()
		if g.notify != notify {
			g.mu.Unlock()
			return // already released by the owner path
		}
		g.held = false
		g.notify = make(chan struct{})
		close(notify)
		g.mu.Unlock()
		g.fireReleaseHook()
	}, true
}

// Acquire blocks until the slot is free or the context is done. The returned
// release function must be called exactly once when the holder reaches a
// terminal state; it wakes every waiter via the broadcast close.
func (g *ExecutionGate) Acquire(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if !g.held {
			g.held = true
			g.mu.Unlock()
			released := false
			return func() {
				g.mu.Lock()
				if released {
					g.mu.Unlock()
					return
				}
				released = true
				g.held = false
				old := g.notify
				g.notify = make(chan struct{})
				close(old)
				g.mu.Unlock()
				g.fireReleaseHook()
			}, nil
		}
		notify := g.notify
		g.mu.Unlock()

		select {
		case <-notify:
			// Slot may be free; re-contend.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
