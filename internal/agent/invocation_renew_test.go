// invocation_renew_test.go verifies the lease-renewal loop's failure policy:
// a definitive server rejection (4xx) stops the local run immediately, and a
// persistently failing (transport-level) renewal stops after the bounded
// retry budget. A recovering renewal keeps the run alive.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// renewTestManager wires an InvocationManager against a fake server whose
// lease-renew endpoint replies with the given status codes in order (the last
// one repeats). The active invocation is installed so the stop path can be
// observed through its state.
func renewTestManager(t *testing.T, statuses ...int) (*InvocationManager, *activeInvocation, context.CancelFunc) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent-invocations/inv-1/lease-renew", func(w http.ResponseWriter, r *http.Request) {
		status := statuses[len(statuses)-1]
		if len(statuses) > 1 {
			status = statuses[0]
			statuses = statuses[1:]
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"renew"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := &Config{Agent: AgentInfo{Name: "alpha"}, Server: ServerConfig{URL: srv.URL}}
	m := NewInvocationManager(cfg, NewClient(srv.URL, "td_test"), NewExecutionGate(), "agent-1", "ws-1", "atomcode")
	m.SetTimingForTest(time.Hour, 10*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	act := &activeInvocation{
		invocationID: "inv-1",
		executionID:  "exec-1",
		cancel:       cancel,
	}
	m.mu.Lock()
	m.active = act
	m.mu.Unlock()
	return m, act, cancel
}

func waitStopCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within the wait window")
}

func TestRenewLoopStopsOnDefinitiveRejection(t *testing.T) {
	// 404: the server no longer knows the ticket — stop immediately, no retries.
	m, act, _ := renewTestManager(t, http.StatusNotFound)
	done := make(chan struct{})
	go func() {
		m.renewLoop(context.Background(), make(chan struct{}), "inv-1", "exec-1")
		close(done)
	}()
	waitStopCond(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return act.cancelRequested
	})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("renewLoop did not exit after the definitive rejection")
	}
}

func TestRenewLoopRetriesTransientThenStops(t *testing.T) {
	// Transport-style failures are represented by the server hanging up: a
	// 5xx is the closest stub. Three consecutive failures must stop the run.
	m, act, _ := renewTestManager(t, http.StatusInternalServerError)
	done := make(chan struct{})
	go func() {
		m.renewLoop(context.Background(), make(chan struct{}), "inv-1", "exec-1")
		close(done)
	}()
	waitStopCond(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return act.cancelRequested
	})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("renewLoop did not exit after the transient budget")
	}
}

func TestRenewLoopSurvivesTransientRecovery(t *testing.T) {
	// 500 once, then 200: the counter resets and the loop keeps running.
	m, act, _ := renewTestManager(t, http.StatusInternalServerError, http.StatusOK, http.StatusOK, http.StatusOK, http.StatusOK)
	done := make(chan struct{})
	go func() {
		m.renewLoop(context.Background(), make(chan struct{}), "inv-1", "exec-1")
		close(done)
	}()
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("renewLoop exited although the renewal recovered")
		default:
		}
		m.mu.Lock()
		requested := act.cancelRequested
		m.mu.Unlock()
		if requested {
			t.Fatalf("a single transient failure must not stop the run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
