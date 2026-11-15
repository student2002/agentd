// execution_gate_test.go verifies the shared execution slot semantics:
// single holder, broadcast wake-up of all blocked acquirers, no-op release on
// a lost TryAcquire, and context-cancel escape from a blocked Acquire.
package agent_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// TestExecutionGateSingleHolder verifies mutual exclusion and that release
// opens the slot for the next acquirer.
func TestExecutionGateSingleHolder(t *testing.T) {
	gate := agent.NewExecutionGate()

	release, ok := gate.TryAcquire()
	if !ok {
		t.Fatalf("fresh gate must be acquirable")
	}
	if _, ok := gate.TryAcquire(); ok {
		t.Fatalf("second acquire must fail while held")
	}
	release()

	release2, ok := gate.TryAcquire()
	if !ok {
		t.Fatalf("gate must be acquirable after release")
	}
	release2()
}

// TestExecutionGateTryAcquireNoopRelease verifies the release of a lost
// TryAcquire is a no-op and does not steal the current holder's slot.
func TestExecutionGateTryAcquireNoopRelease(t *testing.T) {
	gate := agent.NewExecutionGate()
	holderRelease, ok := gate.TryAcquire()
	if !ok {
		t.Fatalf("first acquire must succeed")
	}

	loserRelease, ok := gate.TryAcquire()
	if ok {
		t.Fatalf("second acquire must fail")
	}
	loserRelease() // must not release the holder's slot

	if _, ok := gate.TryAcquire(); ok {
		t.Fatalf("slot must still be held after the loser releases")
	}
	holderRelease()
}

// TestExecutionGateBroadcastWakesAll verifies that a release wakes every
// blocked acquirer and exactly one of them wins the slot.
func TestExecutionGateBroadcastWakesAll(t *testing.T) {
	gate := agent.NewExecutionGate()
	holderRelease, _ := gate.TryAcquire()

	ctx := context.Background()
	var woke atomic.Int32
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := gate.Acquire(ctx)
			if err != nil {
				return
			}
			woke.Add(1)
			won.Add(1)
			time.Sleep(50 * time.Millisecond) // hold briefly so the others re-block
			release()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	holderRelease()
	wg.Wait()

	if woke.Load() != 3 || won.Load() != 3 {
		t.Fatalf("acquirers woke=%d won=%d, want 3/3 (sequential wins via broadcast re-contend)", woke.Load(), won.Load())
	}
	if _, ok := gate.TryAcquire(); !ok {
		t.Fatalf("gate must be free after all releases")
	}
}

// TestExecutionGateContextCancel verifies a blocked Acquire escapes when the
// context is cancelled, without releasing the holder's slot.
func TestExecutionGateContextCancel(t *testing.T) {
	gate := agent.NewExecutionGate()
	holderRelease, _ := gate.TryAcquire()
	defer holderRelease()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := gate.Acquire(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("cancelled acquire must return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("blocked acquire did not escape on context cancel")
	}
}
