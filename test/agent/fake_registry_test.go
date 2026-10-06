// fake_registry_test.go provides a test fake of the RuntimeRegistry backed
// by real AgentRuntime instances, one per (connection, instance name).
package agent_test

import (
	"fmt"
	"strings"
	"sync"

	"github.com/teammate/agentd/internal/agent"
)

type fakeRegistry struct {
	mu          sync.Mutex
	runtimes    map[[2]string]*agent.AgentRuntime // key = {connection, name}
	order       [][2]string
	failNext    error
	connections []agent.ConnectionInfo
}

func newFakeRegistry(conn string, names ...string) *fakeRegistry {
	reg := &fakeRegistry{runtimes: make(map[[2]string]*agent.AgentRuntime)}
	for _, name := range names {
		reg.add(conn, name)
	}
	return reg
}

func (f *fakeRegistry) add(conn, name string) {
	view := &agent.Config{}
	view.Agent.Name = name
	view.Agent.Provider = "claude"
	key := [2]string{conn, name}
	if _, exists := f.runtimes[key]; !exists {
		f.order = append(f.order, key)
	}
	f.runtimes[key] = agent.NewAgentRuntime(view, conn, name)
}

func (f *fakeRegistry) Agents() []agent.LocalAgentInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]agent.LocalAgentInfo, 0, len(f.order))
	for _, key := range f.order {
		rt := f.runtimes[key]
		info := agent.LocalAgentInfo{Connection: key[0], Name: key[1], Provider: "claude", Status: "pending"}
		if rt != nil {
			snap := rt.Snapshot()
			info.AgentID = snap.Config.AgentID
			if info.AgentID != "" {
				info.Status = "online"
			}
		}
		out = append(out, info)
	}
	return out
}

func (f *fakeRegistry) Runtime(connName, name string) (agent.RuntimeHandle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.runtimes[[2]string{connName, name}]
	return rt, ok
}

func (f *fakeRegistry) Connections() []agent.ConnectionInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.ConnectionInfo{}, f.connections...)
}

func (f *fakeRegistry) AddConnection(name, token string) error {
	if !strings.HasPrefix(token, "td_") {
		return fmt.Errorf("daemon token must carry the td_ prefix from the web UI")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		return f.failNext
	}
	if name == "" {
		name = "ws-derived"
	}
	for _, conn := range f.connections {
		if conn.Name == name {
			return fmt.Errorf("workspace %q already exists", name)
		}
	}
	f.connections = append(f.connections, agent.ConnectionInfo{
		Name:        name,
		ServerURL:   "http://127.0.0.1:1",
		TokenMasked: token[:4] + "…",
	})
	return nil
}

func (f *fakeRegistry) RemoveConnection(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		return f.failNext
	}
	kept := make([]agent.ConnectionInfo, 0, len(f.connections))
	found := false
	for _, conn := range f.connections {
		if conn.Name == name {
			found = true
			continue
		}
		kept = append(kept, conn)
	}
	if !found {
		return fmt.Errorf("workspace %q not found", name)
	}
	f.connections = kept
	return nil
}
