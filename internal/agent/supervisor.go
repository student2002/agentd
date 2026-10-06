// supervisor.go implements the machine-level process orchestration.
//
// The Supervisor owns everything machine-scoped: the (connection, instance)
// runtime pool — one AgentRuntime per materialized entry agent, so the same
// instance name on two connections gets two independent runtimes that never
// queue behind each other — the tool-detection loop, the direct-chat manager,
// the local control server, and one WorkspaceConnection per workspaces[]
// entry (each with its own registration loop, SSE stream and heartbeat).
// Instances stay pending until their connection binds their identity in that
// workspace.
//
// Config persistence is single-pointed: every SaveGlobalConfig goes through
// persistConfig under one lock so concurrent connections and control-API
// mutations never interleave writes.
//
// Shutdown order: stop all watchers → stop every connection (SSE, heartbeat,
// session refresher) → deregister each daemon → interrupt and tear down every
// runtime → stop chat → stop the local control server.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/teammate/agentd/internal/agent/tool"
)

// registerRetryInterval is the retry interval while registration fails (e.g.
// the server is not reachable yet).
const registerRetryInterval = 30 * time.Second

// runtimeKey addresses one runtime: the (connection, instance name) pair.
type runtimeKey struct {
	conn string
	name string
}

// LocalAgentInfo is one (connection, instance) summary served by the local
// control API.
type LocalAgentInfo struct {
	Connection string `json:"connection"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	PersonaKey string `json:"persona_key,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	Status     string `json:"status"` // pending | online | busy | paused
}

// ConnectionInfo reports one workspace connection's state to the local
// control API. The daemon token is never returned in full.
type ConnectionInfo struct {
	Name         string   `json:"name"`
	ServerURL    string   `json:"server_url"`
	TokenMasked  string   `json:"token_masked"`
	WorkspaceID  string   `json:"workspace_id"`
	DaemonID     string   `json:"daemon_id"`
	Registered   bool     `json:"registered"`
	SSEConnected bool     `json:"sse_connected"`
	Agents       []string `json:"agents"` // materialized instance names
}

// RuntimeHandle exposes one instance's components to the local control API.
type RuntimeHandle interface {
	Name() string
	Provider() string
	PersonaKey() string
	Busy() bool
	State() *LocalStateStore
	Hub() *LocalEventHub
	Buffer() *LogBuffer
	Executor() *TaskExecutor
	Watchers() []*NodeWatcher
}

// RuntimeRegistry resolves (connection, instance) runtimes and workspace
// connections for the local control API. The instance catalog is
// server-owned — instances are materialized from desired delivery, never
// created locally — so the registry only reads instances and manages
// connections (changes persist the YAML and take effect immediately).
type RuntimeRegistry interface {
	Agents() []LocalAgentInfo
	Runtime(connName, name string) (RuntimeHandle, bool)
	Connections() []ConnectionInfo
	AddConnection(name, token string) error
	RemoveConnection(name string) error
}

// Supervisor manages the full machine lifecycle: connections, the instance
// pool, probing, chat, and the local control API.
type Supervisor struct {
	cfg     *GlobalConfig
	cfgPath string

	probe       *ProbeManager
	chat        *ChatManager
	localServer *LocalServer

	runtimes map[runtimeKey]*AgentRuntime  // key = (connection, instance name)
	conns    map[string]*WorkspaceConnection // key = connection name

	publicKeyPEM  string
	privateKeyPEM string
	toolFactory   func(provider, path string) tool.Tool
	runCtx        context.Context

	cfgMu      sync.Mutex // serializes config mutations + persistence
	mu         sync.Mutex // guards runtimes/conns maps
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// SupervisorOptions carries optional test seams.
type SupervisorOptions struct {
	// ToolFactory replaces the coding-tool factory used by the executor,
	// chat and probing.
	ToolFactory func(provider, path string) tool.Tool
}

// NewSupervisor creates the daemon supervisor with one connection per
// workspaces[] entry.
func NewSupervisor(cfg *GlobalConfig, cfgPath string, opts SupervisorOptions) *Supervisor {
	probe := NewProbeManager(cfg.Tools)
	if opts.ToolFactory != nil {
		probe.SetFactory(opts.ToolFactory)
	}
	chatOptions := ChatManagerOptions{}
	if opts.ToolFactory != nil {
		chatOptions.ToolFactory = opts.ToolFactory
	}
	chat := NewChatManager(cfg.MachineView(), NewLogBuffer(LogBufferDefaultCapacity), nil, chatOptions)

	s := &Supervisor{
		cfg:      cfg,
		cfgPath:  cfgPath,
		probe:    probe,
		chat:     chat,
		toolFactory: opts.ToolFactory,
		runtimes: make(map[runtimeKey]*AgentRuntime),
		conns:    make(map[string]*WorkspaceConnection),
		stopCh:   make(chan struct{}),
	}
	if cfg.Local.Enabled {
		s.localServer = NewLocalServer(LocalServerConfig{
			BindAddr:   cfg.Local.BindAddr,
			LocalToken: cfg.Local.LocalToken,
			Version:    AgentdVersion,
			Chat:       chat,
			Registry:   s,
			Memory:     NewMemoryStore(""),
		})
	}
	// Machine-level RSA key pair for git credential decryption, generated in
	// the constructor so it exists before the connections are built; every
	// connection client receives the private key, and the public key is
	// reported with registration on each connection.
	publicKeyPEM, privateKeyPEM, err := GenerateRSAKeyPair()
	if err != nil {
		log.Printf("[supervisor] WARNING: failed to generate RSA key pair: %v", err)
		log.Printf("[supervisor] Continuing without RSA key pair — git credentials will not be available")
	} else {
		s.publicKeyPEM = publicKeyPEM
		s.privateKeyPEM = privateKeyPEM
		log.Printf("[supervisor] RSA key pair generated for credential decryption")
	}
	// Connection objects exist from construction (identity routing and the
	// control API need them before Run); their registration loops start in
	// Run.
	s.ensureConnections()
	return s
}

// connectionHooks wires one connection to the supervisor's services. The
// instance-addressing hooks close over the connection name so a name
// addresses the (connection, instance) runtime.
func (s *Supervisor) connectionHooks(connName string) ConnectionHooks {
	return ConnectionHooks{
		BuildReport:       s.buildReport,
		ProviderInstalled: s.providerInstalled,
		Runtime:           func(name string) *AgentRuntime { return s.runtimeFor(connName, name) },
		EnsureRuntime:     func(name string) { s.ensureRuntime(connName, name) },
		RemoveRuntime:     func(name string) { s.removeRuntime(connName, name) },
		PersistConfig:     s.persistConfig,
		OnEvent:           s.handleDaemonEvent,
	}
}

// buildReport assembles the registration report from the current probe
// snapshot. The report carries machine information only — the instance
// catalog lives server-side and travels through desired_agents.
func (s *Supervisor) buildReport() RegisterDaemonReport {
	providers := s.probe.LastSnapshot()
	if len(providers) == 0 {
		providers = s.probe.Snapshot()
	}
	return RegisterDaemonReport{
		DeviceName: DeviceName(s.cfg.Name),
		Version:    AgentdVersion,
		PublicKey:  s.publicKeyPEM,
		Providers:  providers,
	}
}

// providerInstalled reports whether the provider passes the local probe
// snapshot. An empty snapshot cannot judge and passes.
func (s *Supervisor) providerInstalled(provider string) bool {
	snapshot := s.probe.LastSnapshot()
	if len(snapshot) == 0 {
		return true
	}
	for _, info := range snapshot {
		if info.Provider == provider {
			return info.Installed
		}
	}
	return false
}

// persistConfig saves the config under the single-point lock so concurrent
// connections and control-API mutations never interleave writes.
func (s *Supervisor) persistConfig() error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := SaveGlobalConfig(s.cfg, s.cfgPath); err != nil {
		return fmt.Errorf("persist config: %w", err)
	}
	return nil
}

// ensureRuntime creates the local runtime for a delivered instance of one
// connection if absent. When the runtime already exists but its view carries
// a persona older than the entry's (a backfill or server-side overwrite
// landed between runs), the runtime is rebuilt: the stale view would keep
// injecting the old memory identity into execution env. The reconciler
// rebinds the identity right after, so the rebuilt runtime comes back online
// with the same agent UUID.
func (s *Supervisor) ensureRuntime(connName, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := runtimeKey{conn: connName, name: name}
	if rt, exists := s.runtimes[key]; exists {
		entry := s.cfg.Workspace(connName)
		if entry == nil {
			return
		}
		current := entry.Agent(name)
		if current == nil || rt.PersonaKey() == current.PersonaKey {
			return
		}
		rt.StopWatchers()
		delete(s.runtimes, key)
		log.Printf("[supervisor] runtime rebuilt for connection=%s agent=%s: persona %s -> %s", connName, name, rt.PersonaKey(), current.PersonaKey)
	}
	s.addRuntimeLocked(connName, name)
}

// removeRuntime tears down the runtime of an instance pruned from the
// delivered full set (web-side deletion): the watcher stops (no new claims)
// and the runtime leaves the pool. An in-flight execution is left to finish
// on its own executor.
func (s *Supervisor) removeRuntime(connName, name string) {
	s.mu.Lock()
	key := runtimeKey{conn: connName, name: name}
	runtime, ok := s.runtimes[key]
	delete(s.runtimes, key)
	s.mu.Unlock()
	if !ok {
		return
	}
	runtime.StopWatchers()
	log.Printf("[supervisor] runtime removed for connection=%s agent=%s (deleted server-side)", connName, name)
}

// addRuntimeLocked creates and registers the runtime for one materialized
// instance of a connection. A creation failure logs and leaves the instance
// dormant (re-delivery retries). Caller must hold s.mu.
func (s *Supervisor) addRuntimeLocked(connName, name string) {
	key := runtimeKey{conn: connName, name: name}
	if _, exists := s.runtimes[key]; exists {
		return
	}
	entry := s.cfg.Workspace(connName)
	if entry == nil {
		log.Printf("[supervisor] cannot create runtime for %q: connection %q not found in config", name, connName)
		return
	}
	agent := entry.Agent(name)
	if agent == nil {
		log.Printf("[supervisor] cannot create runtime: connection %q has no materialized agent %q", connName, name)
		return
	}
	view := s.cfg.viewForAgent(entry, *agent)
	runtime := NewAgentRuntime(view, connName, name)
	if s.toolFactory != nil {
		provider := view.Agent.Provider
		path := ToolPathFor(s.cfg.Tools, provider)
		factory := s.toolFactory
		runtime.SetToolFactory(func() tool.Tool { return factory(provider, path) })
	}
	s.runtimes[key] = runtime
	log.Printf("[supervisor] runtime created for connection=%s agent=%s provider=%s", connName, name, agent.Provider)
}

// Run starts the daemon and blocks until a shutdown signal is received.
//
// Startup flow:
//  1. Start the local control API (optional)
//  2. Generate the machine-level RSA key pair (git credential decryption)
//  3. Probe the coding tools
//  4. Create one runtime per materialized connection agent whose provider is
//     installed locally
//  5. Start one registration loop per workspace connection (30s retry until
//     the server is reachable); each first success opens that connection's
//     SSE stream and heartbeat
//  6. Run the tool re-probe loop (diff → re-register on every connection)
//  7. Wait for SIGINT/SIGTERM, then shut down gracefully
func (s *Supervisor) Run(ctx context.Context) error {
	s.runCtx = ctx
	instanceCount := 0
	for _, w := range s.cfg.Workspaces {
		instanceCount += len(w.Agents)
	}
	log.Printf("[supervisor] starting: workspaces=%d instances=%d", len(s.cfg.Workspaces), instanceCount)

	if s.cfg.Local.Enabled {
		if s.cfg.Local.LocalToken == "" {
			return fmt.Errorf("local.local_token is required when local.enabled=true")
		}
		if s.cfg.Local.InstanceID == "" {
			return fmt.Errorf("local.instance_id is required when local.enabled=true")
		}
		if err := s.localServer.Start(); err != nil {
			return fmt.Errorf("start local control API: %w", err)
		}
		log.Printf("[supervisor] local control API started on %s", s.cfg.Local.BindAddr)
		log.Printf("[supervisor] local control page: http://%s/api/local/control/page?token=%s", s.cfg.Local.BindAddr, s.cfg.Local.LocalToken)
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.localServer.Stop(stopCtx); err != nil {
				log.Printf("[supervisor] WARNING: failed to stop local control API: %v", err)
			}
		}()
	} else {
		log.Printf("[supervisor] local control API disabled (local.enabled=false in the config); no control page will be served")
	}

	// Full tool probe. Snapshot synchronously first: the first registration
	// report reads LastSnapshot(), which stays empty until probed (the
	// re-probe loop ticks only every probeInterval). Instances whose provider
	// is not installed are skipped during runtime creation below — their
	// materialized entries stay dormant until the tool is installed.
	s.probe.Snapshot()

	// Create the runtime pool; instances stay pending (no watcher) until
	// their connection binds their identity.
	if err := s.createStartupRuntimes(); err != nil {
		return err
	}

	// One registration loop per workspace connection.
	for _, conn := range s.startConnectionLoops() {
		conn := conn
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			conn.Run(ctx)
		}()
	}

	// Tool re-probe loop: stopped by the daemon stop channel.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		probeCtx, cancelProbe := context.WithCancel(context.Background())
		defer cancelProbe()
		go func() {
			select {
			case <-probeCtx.Done():
			case <-s.stopCh:
				cancelProbe()
			}
		}()
		s.probe.Run(probeCtx, func(changed []ProviderInfo) {
			s.triggerRegisterAll()
		})
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("[supervisor] received signal %v, shutting down...", sig)
	case <-s.stopCh:
		log.Printf("[supervisor] stop requested, shutting down...")
	case <-ctx.Done():
		log.Printf("[supervisor] context cancelled, shutting down...")
	}

	s.shutdown()
	log.Printf("[supervisor] shutdown complete")
	return nil
}

// ensureConnections builds the connection objects for the configured entries
// if absent. Idempotent: existing connections are kept.
func (s *Supervisor) ensureConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Workspaces {
		entry := &s.cfg.Workspaces[i]
		if _, exists := s.conns[entry.Name]; exists {
			continue
		}
		conn := NewWorkspaceConnection(s.cfg, entry, s.connectionHooks(entry.Name))
		conn.SetPrivateKeyPEM(s.privateKeyPEM)
		s.conns[entry.Name] = conn
		log.Printf("[supervisor] connection created for workspace=%s", entry.Name)
	}
}

// startConnectionLoops launches the registration loop of every configured
// connection that is not running yet.
func (s *Supervisor) startConnectionLoops() []*WorkspaceConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := make([]*WorkspaceConnection, 0, len(s.conns))
	for _, conn := range s.conns {
		if conn.Started() {
			continue
		}
		conn.MarkStarted()
		started = append(started, conn)
	}
	return started
}

// Stop requests a graceful shutdown (idempotent).
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// shutdown runs the graceful shutdown sequence.
func (s *Supervisor) shutdown() {
	s.Stop()

	// 1. Stop all watchers first: no instance claims new nodes.
	for _, rt := range s.snapshotRuntimes() {
		rt.StopWatchers()
	}
	// 2. Stop every connection (SSE stream, heartbeat, session refresher,
	// registration loop) and deregister its daemon.
	for _, conn := range s.snapshotConnections() {
		conn.Stop()
		if conn.Registered() {
			deregisterCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := conn.Client().DaemonDeregister(deregisterCtx); err != nil {
				log.Printf("[supervisor] WARNING: deregister failed for workspace %s: %v", conn.Name(), err)
			} else {
				log.Printf("[supervisor] daemon deregistered on workspace %s; instances offline there", conn.Name())
			}
			cancel()
		}
	}
	// 3. Interrupt in-flight executions and tear each runtime down.
	for _, rt := range s.snapshotRuntimes() {
		rt.InterruptRunning()
		rt.Stop()
	}
	// 4. Machine-level components.
	s.chat.Stop()
	log.Printf("[supervisor] chat manager stopped")
	s.wg.Wait()
}

// createStartupRuntimes creates one runtime per materialized connection agent
// whose provider is installed per the probe snapshot; the rest are skipped
// with a warning so a single uninstalled provider cannot keep the daemon down.
func (s *Supervisor) createStartupRuntimes() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Workspaces {
		entry := &s.cfg.Workspaces[i]
		for _, agent := range entry.Agents {
			if !s.providerInstalled(agent.Provider) {
				log.Printf("[supervisor] WARNING: agent %q on connection %q skipped: provider %q is not installed locally", agent.Name, entry.Name, agent.Provider)
				continue
			}
			s.addRuntimeLocked(entry.Name, agent.Name)
		}
	}
	return nil
}

// triggerRegisterAll requests one immediate re-registration on every
// connection (probe diff refreshes the providers snapshot in the report).
func (s *Supervisor) triggerRegisterAll() {
	for _, conn := range s.snapshotConnections() {
		conn.TriggerRegister()
	}
}

// handleDaemonEvent routes one daemon-stream event arriving on a connection:
// the envelope agent_id selects the target instance within that connection's
// instance list; events without one are broadcast to every instance
// materialized on the connection (e.g. sync:required). Events for unknown
// identities are dropped with a log line (e.g. an identity not yet resolved
// during adoption).
func (s *Supervisor) handleDaemonEvent(conn *WorkspaceConnection, eventType string, data json.RawMessage) {
	var envelope struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.AgentID != "" {
		name, mapped := conn.NameForUUID(envelope.AgentID)
		var runtime *AgentRuntime
		if mapped {
			runtime = s.runtimeFor(conn.Name(), name)
		}
		if runtime == nil {
			log.Printf("[supervisor] SSE event %s for unknown agent_id %s on connection %s dropped", eventType, envelope.AgentID, conn.Name())
			return
		}
		runtime.HandleEvent(eventType, data)
		return
	}

	for _, name := range conn.AgentNames() {
		if runtime := s.runtimeFor(conn.Name(), name); runtime != nil {
			runtime.HandleEvent(eventType, data)
		}
	}
}

func (s *Supervisor) snapshotRuntimes() []*AgentRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*AgentRuntime, 0, len(s.runtimes))
	for _, rt := range s.runtimes {
		out = append(out, rt)
	}
	return out
}

func (s *Supervisor) snapshotConnections() []*WorkspaceConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*WorkspaceConnection, 0, len(s.conns))
	for _, conn := range s.conns {
		out = append(out, conn)
	}
	return out
}

func (s *Supervisor) runtimeFor(connName, name string) *AgentRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtimes[runtimeKey{conn: connName, name: name}]
}

// --- RuntimeRegistry implementation (local control API) ---

// Agents lists every (connection, instance) pair with its live status.
func (s *Supervisor) Agents() []LocalAgentInfo {
	s.mu.Lock()
	runtimes := make(map[runtimeKey]*AgentRuntime, len(s.runtimes))
	for key, rt := range s.runtimes {
		runtimes[key] = rt
	}
	conns := make(map[string]*WorkspaceConnection, len(s.conns))
	for name, conn := range s.conns {
		conns[name] = conn
	}
	s.mu.Unlock()

	out := make([]LocalAgentInfo, 0)
	for i := range s.cfg.Workspaces {
		entry := &s.cfg.Workspaces[i]
		// The live connection serializes entry reads against reconcile
		// mutations; entries without a connection object are read directly
		// (they are not being mutated — no registration loop runs).
		agents := append([]WorkspaceAgent{}, entry.Agents...)
		if conn := conns[entry.Name]; conn != nil {
			agents = conn.agentsSnapshot()
		}
		for _, a := range agents {
			info := LocalAgentInfo{
				Connection: entry.Name,
				Name:       a.Name,
				Provider:   a.Provider,
				PersonaKey: a.PersonaKey,
				AgentID:    a.AgentID,
				Status:     "pending",
			}
			if rt := runtimes[runtimeKey{conn: entry.Name, name: a.Name}]; rt != nil {
				info.AgentID = rt.Snapshot().Config.AgentID
				watchers := rt.Watchers()
				paused := false
				for _, watcher := range watchers {
					if watcher.IsPaused() {
						paused = true
						break
					}
				}
				switch {
				case paused:
					info.Status = "paused"
				case rt.Busy():
					info.Status = "busy"
				case info.AgentID != "":
					info.Status = "online"
				}
			}
			out = append(out, info)
		}
	}
	return out
}

// Runtime resolves one (connection, instance) pair.
func (s *Supervisor) Runtime(connName, name string) (RuntimeHandle, bool) {
	rt := s.runtimeFor(connName, name)
	return rt, rt != nil
}

// Connections lists every workspace connection with its live state.
func (s *Supervisor) Connections() []ConnectionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ConnectionInfo, 0, len(s.cfg.Workspaces))
	for i := range s.cfg.Workspaces {
		entry := &s.cfg.Workspaces[i]
		info := ConnectionInfo{
			Name:        entry.Name,
			ServerURL:   s.cfg.Server.URL,
			TokenMasked: maskDaemonToken(entry.Token),
			WorkspaceID: entry.WorkspaceID,
			DaemonID:    entry.DaemonID,
		}
		if conn := s.conns[entry.Name]; conn != nil {
			info.Registered = conn.Registered()
			info.SSEConnected = conn.SSEConnected()
			info.Agents = conn.AgentNames()
			if info.WorkspaceID == "" {
				info.WorkspaceID = conn.WorkspaceID()
			}
			if info.DaemonID == "" {
				info.DaemonID = conn.DaemonID()
			}
		} else {
			for _, a := range entry.Agents {
				info.Agents = append(info.Agents, a.Name)
			}
		}
		out = append(out, info)
	}
	return out
}

// AddConnection registers a new workspace connection: the entry is appended,
// persisted, and its registration loop starts immediately. An empty name is
// derived from the token. A connection is name + token only — its instances
// arrive through desired delivery.
func (s *Supervisor) AddConnection(name, token string) error {
	token = trimSpace(token)
	if !strings.HasPrefix(token, "td_") {
		return fmt.Errorf("daemon token must carry the td_ prefix from the web UI")
	}
	name = trimSpace(name)
	if name == "" {
		name = DeriveWorkspaceName(token)
	}

	s.cfgMu.Lock()
	if s.cfg.Workspace(name) != nil {
		s.cfgMu.Unlock()
		return fmt.Errorf("workspace %q already exists", name)
	}
	s.cfg.Workspaces = append(s.cfg.Workspaces, WorkspaceEntry{Name: name, Token: token})
	if err := SaveGlobalConfig(s.cfg, s.cfgPath); err != nil {
		if idx := len(s.cfg.Workspaces) - 1; idx >= 0 {
			s.cfg.Workspaces = s.cfg.Workspaces[:idx]
		}
		s.cfgMu.Unlock()
		return fmt.Errorf("persist config: %w", err)
	}
	s.cfgMu.Unlock()

	s.ensureConnections()
	if s.runCtx != nil {
		for _, conn := range s.startConnectionLoops() {
			conn := conn
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				conn.Run(s.runCtx)
			}()
		}
	}
	log.Printf("[supervisor] workspace connection %q added; registering", name)
	return nil
}

// RemoveConnection stops the connection (SSE, heartbeat, identities) and
// removes its entry and runtimes from the pool. Sibling connections are
// unaffected.
func (s *Supervisor) RemoveConnection(name string) error {
	s.mu.Lock()
	conn := s.conns[name]
	delete(s.conns, name)
	removed := make([]*AgentRuntime, 0, 4)
	for key, rt := range s.runtimes {
		if key.conn == name {
			removed = append(removed, rt)
			delete(s.runtimes, key)
		}
	}
	s.mu.Unlock()
	if conn == nil && s.cfg.Workspace(name) == nil {
		return fmt.Errorf("workspace %q not found", name)
	}
	if conn != nil {
		conn.Stop()
	}
	// The removed connection's instances stop claiming; in-flight executions
	// keep running to completion on their own.
	for _, rt := range removed {
		rt.StopWatchers()
	}

	s.cfgMu.Lock()
	filtered := make([]WorkspaceEntry, 0, len(s.cfg.Workspaces))
	for _, w := range s.cfg.Workspaces {
		if w.Name != name {
			filtered = append(filtered, w)
		}
	}
	s.cfg.Workspaces = filtered
	if err := SaveGlobalConfig(s.cfg, s.cfgPath); err != nil {
		s.cfgMu.Unlock()
		return fmt.Errorf("persist config: %w", err)
	}
	s.cfgMu.Unlock()

	// The rebuild above moved every surviving entry to a new backing array;
	// re-point the surviving connections so their in-place mutations (and
	// reconciliation prunes) keep reaching the persisted config.
	for _, conn := range s.snapshotConnections() {
		if entry := s.cfg.Workspace(conn.Name()); entry != nil {
			conn.RebindEntry(entry)
		}
	}
	log.Printf("[supervisor] workspace connection %q removed", name)
	return nil
}

func (s *Supervisor) connectionByName(name string) *WorkspaceConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[name]
}

// ResolveIdentityForTest wires one instance's identity inside a connection the
// way a successful registration does, without network access. Test-only seam.
func (s *Supervisor) ResolveIdentityForTest(connName, name, agentID, workspaceID string) {
	conn := s.connectionByName(connName)
	if conn == nil {
		return
	}
	conn.ResolveIdentityForTest(name, agentID, workspaceID)
}

// EnsureRuntimeForTest creates the local runtime for one materialized
// instance the way desired delivery does, without network access. Test-only
// seam.
func (s *Supervisor) EnsureRuntimeForTest(connName, name string) {
	s.ensureRuntime(connName, name)
}

// HandleDaemonEventForTest routes one daemon-stream event through the
// supervisor's dispatcher. Test-only seam.
func (s *Supervisor) HandleDaemonEventForTest(connName, eventType string, data json.RawMessage) {
	if conn := s.connectionByName(connName); conn != nil {
		s.handleDaemonEvent(conn, eventType, data)
	}
}

// maskDaemonToken returns a display-safe form of the daemon token.
func maskDaemonToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 10 {
		return token[:4] + "…"
	}
	return token[:6] + "…" + token[len(token)-4:]
}

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}
