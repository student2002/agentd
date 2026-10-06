// connection.go implements one remote-workspace connection.
//
// A WorkspaceConnection owns everything scoped to a single workspace entry:
// its dedicated Client (td_ token), the registration loop with 30s retry, the
// daemon SSE stream, the daemon heartbeat, and the connection's materialized
// instance list (entry.Agents — names plus the server agent UUID each name
// resolved inside this workspace). The entry is the identity source: the same
// instance name on another connection is a different entry item and a
// different runtime.
//
// Lifecycle: Run blocks in the registration loop until the context is
// cancelled or Stop is called; Stop tears down the registration loop, the SSE
// stream, the heartbeat and the session-token refresher of this connection
// only — the process and sibling connections are unaffected.
package agent

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"
)

// ConnectionHooks are the supervisor services a connection depends on. The
// Runtime/EnsureRuntime hooks are bound to this connection (the supervisor
// closes over the connection name), so an instance name addresses the
// (connection, instance) runtime.
type ConnectionHooks struct {
	// BuildReport assembles the registration report (providers snapshot,
	// device name, public key). The report carries no instance catalog.
	BuildReport func() RegisterDaemonReport
	// ProviderInstalled reports whether a provider passes the local probe
	// snapshot; uninstalled providers are not materialized from desired
	// delivery.
	ProviderInstalled func(provider string) bool
	// Runtime resolves one of this connection's instances; nil when absent.
	Runtime func(name string) *AgentRuntime
	// EnsureRuntime creates the local runtime for a delivered instance of
	// this connection.
	EnsureRuntime func(name string)
	// RemoveRuntime tears down the runtime of an instance pruned from the
	// delivered full set (web-side deletion).
	RemoveRuntime func(name string)
	// PersistConfig serializes a config save through the supervisor's
	// single-point lock (concurrent connections must not interleave writes).
	PersistConfig func() error
	// OnEvent routes one daemon-stream event (the supervisor dispatches on
	// the envelope agent_id within this connection's instance list).
	OnEvent func(conn *WorkspaceConnection, eventType string, data json.RawMessage)
}

// WorkspaceConnection is one workspace's registration/SSE/heartbeat stack.
type WorkspaceConnection struct {
	cfg   *GlobalConfig
	name  string          // entry.Name, the local connection alias
	entry *WorkspaceEntry // points into cfg; adoptions write back in place
	client *Client
	hooks  ConnectionHooks

	registrar  *Registrar
	reconciler *Reconciler

	// entryMu serializes entry.Agents mutations (reconcile materialization,
	// identity writes) with the readers (heartbeat busy list, event routing).
	// Callers must not hold it while invoking the supervisor hooks — the
	// hooks take the supervisor lock, so the order is always entryMu →
	// supervisor locks.
	entryMu sync.Mutex

	mu          sync.Mutex
	workspaceID string
	daemonID    string
	registered  bool
	started     bool
	sse         *SSEClient
	hb          *DaemonHeartbeat

	registerCh chan struct{}
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// NewWorkspaceConnection builds the connection for one workspace entry. The
// entry must live inside cfg; identity adoption and workspace/daemon id
// adoption mutate it in place.
func NewWorkspaceConnection(cfg *GlobalConfig, entry *WorkspaceEntry, hooks ConnectionHooks) *WorkspaceConnection {
	conn := &WorkspaceConnection{
		cfg:        cfg,
		name:       entry.Name,
		entry:      entry,
		client:     NewClient(cfg.Server.URL, entry.Token),
		hooks:      hooks,
		registerCh: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
	}
	conn.registrar = NewRegistrar(conn.client, hooks.BuildReport)
	conn.reconciler = NewReconciler(cfg, entry, hooks.PersistConfig, conn.reconcilerHooks())
	return conn
}

// reconcilerHooks builds the reconciler hook set bound to this connection.
// ResolveIdentity runs with entryMu held (applyRegistration and the heartbeat
// callback take it), so it uses the lock-free variant.
func (c *WorkspaceConnection) reconcilerHooks() ReconcilerHooks {
	return ReconcilerHooks{
		ProviderInstalled: c.hooks.ProviderInstalled,
		EnsureRuntime:     c.hooks.EnsureRuntime,
		ResolveIdentity:   c.resolveIdentityLocked,
		RemoveRuntime:     c.hooks.RemoveRuntime,
	}
}

// RebindEntry re-points the connection at its (moved) entry inside
// cfg.Workspaces. Removing one workspace entry rebuilds that slice, leaving
// surviving connections' entry pointers aimed at the stale backing array —
// without rebinding, their in-place mutations would never reach the persisted
// config.
func (c *WorkspaceConnection) RebindEntry(entry *WorkspaceEntry) {
	c.entryMu.Lock()
	defer c.entryMu.Unlock()
	c.entry = entry
	c.reconciler = NewReconciler(c.cfg, entry, c.hooks.PersistConfig, c.reconcilerHooks())
}

// Name returns the local connection alias.
func (c *WorkspaceConnection) Name() string { return c.name }

// Client returns the connection's dedicated client.
func (c *WorkspaceConnection) Client() *Client { return c.client }

// ServerURL returns the server endpoint this connection talks to.
func (c *WorkspaceConnection) ServerURL() string { return c.cfg.Server.URL }

// WorkspaceID returns the workspace id adopted from registration ("" before).
func (c *WorkspaceConnection) WorkspaceID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workspaceID
}

// DaemonID returns the daemon id adopted from registration ("" before).
func (c *WorkspaceConnection) DaemonID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.daemonID
}

// Registered reports whether a registration has succeeded.
func (c *WorkspaceConnection) Registered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registered
}

// Started reports whether the registration loop has been launched.
func (c *WorkspaceConnection) Started() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// MarkStarted flags the registration loop as launched (supervisor bookkeeping
// so Run does not double-start a connection).
func (c *WorkspaceConnection) MarkStarted() {
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
}

// SSEConnected reports whether the daemon event stream is up.
func (c *WorkspaceConnection) SSEConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sse != nil && c.sse.IsConnected()
}

// SetPrivateKeyPEM installs the machine-level RSA private key used to decrypt
// this workspace's git credentials.
func (c *WorkspaceConnection) SetPrivateKeyPEM(pem string) {
	c.client.PrivateKeyPEM = pem
}

// agentsSnapshot copies the connection's materialized instance list.
func (c *WorkspaceConnection) agentsSnapshot() []WorkspaceAgent {
	c.entryMu.Lock()
	defer c.entryMu.Unlock()
	return append([]WorkspaceAgent{}, c.entry.Agents...)
}

// AgentNames lists the names of this connection's materialized instances.
func (c *WorkspaceConnection) AgentNames() []string {
	agents := c.agentsSnapshot()
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return names
}

// NameForUUID resolves a server agent UUID to the local instance name within
// this connection's instance list.
func (c *WorkspaceConnection) NameForUUID(agentUUID string) (string, bool) {
	for _, a := range c.agentsSnapshot() {
		if a.AgentID == agentUUID && a.AgentID != "" {
			return a.Name, true
		}
	}
	return "", false
}

// UUIDForName resolves a local instance name to this workspace's agent UUID.
func (c *WorkspaceConnection) UUIDForName(name string) (string, bool) {
	c.entryMu.Lock()
	defer c.entryMu.Unlock()
	agent := c.entry.Agent(name)
	if agent == nil || agent.AgentID == "" {
		return "", false
	}
	return agent.AgentID, true
}

// TriggerRegister requests one immediate re-registration on this connection.
func (c *WorkspaceConnection) TriggerRegister() {
	select {
	case <-c.stopCh:
	case c.registerCh <- struct{}{}:
	default:
		// A request is already pending.
	}
}

// Run performs the initial registration (retrying every 30s until the server
// is reachable) and handles re-registration triggers. After the first success
// it re-registers only on demand — liveness is the heartbeat's job.
func (c *WorkspaceConnection) Run(ctx context.Context) {
	for {
		resp, err := c.registrar.Register(ctx)
		if err != nil {
			// A 401/404 on registration means the daemon credentials were
			// revoked or the daemon deleted server-side; retrying every 30s
			// cannot recover. Tear the connection down like the heartbeat
			// 401/404 path does.
			if IsNotFound(err) || IsUnauthorized(err) {
				log.Printf("[connection=%s] ERROR: daemon rejected by server (%v) — it was deleted or its token revoked on the web; re-provision the daemon from that workspace's web UI, remove this connection and re-add it with the new token", c.name, err)
				c.onDaemonDeleted()
				c.Stop()
				return
			}
			log.Printf("[connection=%s] WARNING: daemon registration failed: %v", c.name, err)
		} else {
			c.applyRegistration(ctx, resp)
		}

		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-time.After(registerRetryInterval):
		case <-c.registerCh:
		}
	}
}

// Stop tears this connection down: registration loop, SSE stream, heartbeat
// and session-token refresher. Idempotent.
func (c *WorkspaceConnection) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
	c.mu.Lock()
	sse := c.sse
	hb := c.hb
	c.mu.Unlock()
	if sse != nil {
		sse.Stop()
	}
	if hb != nil {
		hb.Stop()
	}
}

// applyRegistration consumes one successful register response: workspace/daemon
// id adoption (persisted to the entry), rebinding of the identities persisted
// for this connection, desired-delivery materialization, the session token
// exchange, and the one-time SSE/heartbeat startup.
func (c *WorkspaceConnection) applyRegistration(ctx context.Context, resp *RegisterDaemonResponse) {
	c.adoptIDs(ctx, resp)

	// Rebind and reconcile serialize on entryMu so the heartbeat's busy
	// list and the event routing never observe a half-written entry. The
	// full set is declarative: an empty set prunes every locally
	// materialized instance, so Reconcile runs unconditionally.
	c.entryMu.Lock()
	c.rebindPersistedIdentities()
	c.reconciler.Reconcile(resp.DesiredAgents)
	c.entryMu.Unlock()

	// Session token: exchange once per connection, then keep refreshing in
	// the background on the connection's stop channel.
	c.mu.Lock()
	firstRegistration := !c.registered
	c.registered = true
	c.mu.Unlock()

	if firstRegistration {
		if err := c.exchangeSessionToken(ctx); err != nil {
			log.Printf("[connection=%s] WARNING: failed to exchange session token: %v (continuing with daemon token)", c.name, err)
		}
		c.startSSE()
		c.startHeartbeat(resp.HeartbeatInterval)
	}
}

// adoptIDs takes the workspace/daemon ids from the register response. The ids
// are authoritative server-side values; a change is written back to the config
// entry and persisted.
func (c *WorkspaceConnection) adoptIDs(ctx context.Context, resp *RegisterDaemonResponse) {
	c.mu.Lock()
	c.daemonID = resp.DaemonID
	c.workspaceID = resp.WorkspaceID
	c.mu.Unlock()

	if resp.WorkspaceID != "" && c.entry.WorkspaceID != resp.WorkspaceID {
		c.entry.WorkspaceID = resp.WorkspaceID
		if err := c.hooks.PersistConfig(); err != nil {
			log.Printf("[connection=%s] WARNING: failed to persist adopted workspace_id: %v", c.name, err)
		} else {
			log.Printf("[connection=%s] adopted workspace_id=%s (persisted)", c.name, resp.WorkspaceID)
		}
	}
	if resp.DaemonID != "" && c.entry.DaemonID != resp.DaemonID {
		c.entry.DaemonID = resp.DaemonID
		if err := c.hooks.PersistConfig(); err != nil {
			log.Printf("[connection=%s] WARNING: failed to persist adopted daemon_id: %v", c.name, err)
		}
	}
}

// rebindPersistedIdentities restores the agent_ids persisted on the connection
// entry into their runtimes. Delivery is incremental — rows leave desired once
// online — so a restart rebuilds identity exclusively from this cache. The
// caller must hold entryMu.
func (c *WorkspaceConnection) rebindPersistedIdentities() {
	for _, agent := range c.agentsSnapshotLocked() {
		if agent.AgentID == "" {
			continue
		}
		c.resolveIdentityLocked(agent.Name, agent.AgentID)
	}
}

// agentsSnapshotLocked copies the materialized list; caller holds entryMu.
func (c *WorkspaceConnection) agentsSnapshotLocked() []WorkspaceAgent {
	return append([]WorkspaceAgent{}, c.entry.Agents...)
}

// resolveIdentityLocked binds one instance's server identity inside this
// connection: it updates the entry's agent_id, persists the change, and
// resolves the identity into the (connection, instance) runtime (starting its
// watcher), warning when the UUID changed (the instance was deleted web-side
// and re-created). Same-UUID repeats only re-resolve into the runtime, which
// deduplicates them. The caller must hold entryMu.
func (c *WorkspaceConnection) resolveIdentityLocked(name, agentID string) {
	agent := c.entry.Agent(name)
	if agent == nil {
		log.Printf("[connection=%s] identity for agent %q has no materialized entry", c.name, name)
		return
	}
	previousUUID := agent.AgentID
	if previousUUID != agentID {
		agent.AgentID = agentID
		if err := c.hooks.PersistConfig(); err != nil {
			log.Printf("[connection=%s] WARNING: failed to persist identity for agent %q: %v", c.name, name, err)
		}
	}
	if previousUUID != "" && previousUUID != agentID {
		log.Printf("[connection=%s] WARNING: agent %q server identity changed %s -> %s (instance was deleted on the web and re-created; prior configuration was lost)", c.name, name, previousUUID, agentID)
	}

	c.mu.Lock()
	workspaceID := c.workspaceID
	c.mu.Unlock()

	runtime := c.hooks.Runtime(name)
	if runtime == nil {
		log.Printf("[connection=%s] delivered agent %q has no local runtime", c.name, name)
		return
	}
	runtime.BindIdentity(agentID, c.client, workspaceID)
}

// exchangeSessionToken exchanges the daemon token for a session token and
// starts the per-connection refresher.
func (c *WorkspaceConnection) exchangeSessionToken(ctx context.Context) error {
	sessionToken, expiresAt, err := c.client.ExchangeToken(ctx, c.client.APIToken)
	if err != nil {
		return err
	}
	c.client.SessionToken = sessionToken
	c.client.SessionExpiry = expiresAt
	log.Printf("[connection=%s] session token obtained, expires at %v", c.name, expiresAt)
	c.client.StartSessionTokenRefresher(c.stopCh)
	return nil
}

// startSSE opens this connection's daemon event stream. Requires daemonID and
// workspaceID from a completed registration.
func (c *WorkspaceConnection) startSSE() {
	c.mu.Lock()
	daemonID := c.daemonID
	workspaceID := c.workspaceID
	c.mu.Unlock()
	if daemonID == "" || workspaceID == "" {
		log.Printf("[connection=%s] WARNING: cannot start SSE without daemon/workspace id", c.name)
		return
	}
	select {
	case <-c.stopCh:
		return
	default:
	}
	sse := NewSSEClientWithCallbacks(
		c.cfg.Server.URL,
		workspaceID,
		daemonID,
		c.client.authToken,
		func(eventType string, data json.RawMessage) {
			c.hooks.OnEvent(c, eventType, data)
		},
		SSECallbacks{
			OnConnected: func(lastEventID string) {
				c.forEachRuntime(func(rt *AgentRuntime) {
					rt.SetSSE(true, lastEventID)
				})
			},
			OnDisconnected: func(err error) {
				c.forEachRuntime(func(rt *AgentRuntime) {
					rt.SetSSE(false, "")
				})
			},
		},
	)
	c.mu.Lock()
	c.sse = sse
	c.mu.Unlock()
	if err := sse.Start(); err != nil {
		log.Printf("[connection=%s] WARNING: failed to start SSE client: %v", c.name, err)
		return
	}
	log.Printf("[connection=%s] SSE client started on daemon stream", c.name)
}

// startHeartbeat starts this connection's heartbeat reporting the busy status
// of its identity-bound agents; responses feed the connection's reconcile
// loop. intervalSecs is the server's preference from the register response
// (<=0 keeps the default); heartbeat responses may retune it later. A 404
// (daemon deleted server-side) stops this connection alone.
func (c *WorkspaceConnection) startHeartbeat(intervalSecs int) {
	select {
	case <-c.stopCh:
		return
	default:
	}
	interval := defaultHeartbeatInterval
	if intervalSecs > 0 {
		interval = time.Duration(intervalSecs) * time.Second
	}
	hb := NewDaemonHeartbeat(c.client, interval, c.busyStatuses, func(resp *DaemonHeartbeatResponse) {
		c.entryMu.Lock()
		c.reconciler.Reconcile(resp.DesiredAgents)
		c.entryMu.Unlock()
	}, HeartbeatCallbacks{
		OnSuccess: func(at time.Time) {
			c.forEachRuntime(func(rt *AgentRuntime) {
				rt.SetHeartbeat(nil)
			})
		},
		OnError: func(err error) {
			if IsNotFound(err) || IsUnauthorized(err) {
				c.onDaemonDeleted()
				return
			}
			c.forEachRuntime(func(rt *AgentRuntime) {
				rt.SetHeartbeat(err)
			})
		},
	})
	c.mu.Lock()
	c.hb = hb
	c.mu.Unlock()
	hb.Start()
	log.Printf("[connection=%s] heartbeat started", c.name)
}

// onDaemonDeleted handles the heartbeat 401/404: the daemon's credentials
// were revoked or the daemon row was deleted server-side (web UI). The
// connection's identities — the agent_ids persisted on the entry — are
// dropped and its heartbeat has stopped itself; claiming and reporting keep
// failing until the operator re-provisions the daemon. The process and
// sibling connections are unaffected.
func (c *WorkspaceConnection) onDaemonDeleted() {
	c.mu.Lock()
	c.registered = false
	c.hb = nil
	c.mu.Unlock()

	c.entryMu.Lock()
	changed := false
	for i := range c.entry.Agents {
		if c.entry.Agents[i].AgentID != "" {
			c.entry.Agents[i].AgentID = ""
			changed = true
		}
	}
	c.entryMu.Unlock()
	if changed {
		if err := c.hooks.PersistConfig(); err != nil {
			log.Printf("[connection=%s] WARNING: failed to persist identity cleanup: %v", c.name, err)
		}
	}
	c.forEachRuntime(func(rt *AgentRuntime) {
		rt.StopWatchers()
		rt.State().ClearAgentIdentity()
	})
	log.Printf("[connection=%s] ERROR: daemon rejected by server (404) — it was deleted on the web; re-provision the daemon from that workspace's web UI, remove this connection and re-add it with the new token", c.name)
}

// busyStatuses collects the per-instance busy payload for the instances this
// connection has bound identities for — delivered via desired or restored from
// the persisted agent_ids. Instances not bound on this connection are not
// reported: the server's absent-offline rule only owns the rows it delivered.
func (c *WorkspaceConnection) busyStatuses() []AgentBusy {
	agents := c.agentsSnapshot()
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.AgentID != "" {
			names = append(names, a.Name)
		}
	}
	sort.Strings(names)
	out := make([]AgentBusy, 0, len(names))
	for _, name := range names {
		if runtime := c.hooks.Runtime(name); runtime != nil {
			out = append(out, AgentBusy{Name: name, Busy: runtime.Busy()})
		}
	}
	return out
}

// forEachRuntime invokes fn on each materialized instance's runtime.
func (c *WorkspaceConnection) forEachRuntime(fn func(*AgentRuntime)) {
	for _, name := range c.AgentNames() {
		if runtime := c.hooks.Runtime(name); runtime != nil {
			fn(runtime)
		}
	}
}

// ResolveIdentityForTest wires one instance's identity inside this connection
// the way a successful registration does, without network access. Test-only
// seam.
func (c *WorkspaceConnection) ResolveIdentityForTest(name, agentID, workspaceID string) {
	c.mu.Lock()
	c.workspaceID = workspaceID
	c.mu.Unlock()
	c.entryMu.Lock()
	if agent := c.entry.Agent(name); agent != nil {
		agent.AgentID = agentID
	}
	c.entryMu.Unlock()
	if runtime := c.hooks.Runtime(name); runtime != nil {
		runtime.BindIdentity(agentID, c.client, workspaceID)
	}
}
