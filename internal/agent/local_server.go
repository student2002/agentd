// local_server.go provides the loopback local control API.
//
// Machine-level endpoints (health, direct chat, the console page) are served
// directly; per-instance endpoints are namespaced under
// /api/local/connections/{conn}/agents/{name}/... and resolved through the
// RuntimeRegistry, so one listener serves every (connection, instance) pair
// of the daemon.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teammate/agentd/web"
)

type LocalServerConfig struct {
	BindAddr   string
	LocalToken string
	Version    string

	// Registry resolves (connection, instance) runtimes and manages workspace
	// connections. Required for the agents and connections endpoints.
	Registry RuntimeRegistry
	// Chat powers the direct-chat endpoints. Optional; when nil the chat
	// endpoints return 503.
	Chat *ChatManager
	// Memory backs the per-instance memory management endpoints. Optional;
	// when nil those endpoints return 503.
	Memory *MemoryStore
}

type LocalServer struct {
	cfg    LocalServerConfig
	server *http.Server
}

func NewLocalServer(cfg LocalServerConfig) *LocalServer {
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1:17380"
	}
	return &LocalServer{cfg: cfg}
}

func (s *LocalServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// Machine-level.
	mux.HandleFunc("/api/local/health", s.getOnly(s.handleHealth))
	mux.HandleFunc("/api/local/control/page", s.getOnly(s.handleControlPage))
	mux.Handle("/api/local/control/assets/", s.getOnlyHandler(http.StripPrefix("/api/local/control/assets/", http.FileServer(http.FS(web.ControlAssets())))))
	mux.HandleFunc("/api/local/chat/tools", s.getOnly(s.requireLocalToken(s.handleChatTools)))
	mux.HandleFunc("/api/local/chat/sessions", s.getOnly(s.requireLocalToken(s.handleChatSessions)))
	mux.HandleFunc("/api/local/chat/session", s.getOnly(s.requireLocalToken(s.handleChatSessionMessages)))
	mux.HandleFunc("/api/local/chat/state", s.getOnly(s.requireLocalToken(s.handleChatState)))
	mux.HandleFunc("/api/local/chat/send", s.postOnly(s.requireLocalToken(s.handleChatSend)))
	mux.HandleFunc("/api/local/chat/stop", s.postOnly(s.requireLocalToken(s.handleChatStop)))
	mux.HandleFunc("/api/local/chat/reset", s.postOnly(s.requireLocalToken(s.handleChatReset)))
	mux.HandleFunc("/api/local/chat/logs/recent", s.getOnly(s.requireLocalToken(s.handleChatLogsRecent)))
	mux.HandleFunc("/api/local/chat/logs/stream", s.getOnly(s.requireLocalToken(s.handleChatLogsStream)))
	mux.HandleFunc("/api/local/connections", s.requireLocalToken(s.handleConnections))
	mux.HandleFunc("/api/local/connections/{name}", s.deleteOnly(s.requireLocalToken(s.handleConnectionDelete)))
	mux.HandleFunc("/api/local/memory/{persona}", s.requireLocalToken(s.handleInstanceMemory))

	// Instance-level, addressed by (connection, name). The instance catalog
	// is server-owned: the list and the runtime/control subroutes are
	// read/operate-only — there is no local instance creation or removal.
	mux.HandleFunc("/api/local/agents", s.getOnly(s.requireLocalToken(s.handleAgentsList)))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/runtime/snapshot", s.getOnly(s.requireLocalToken(s.agentScoped(s.handleSnapshot))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/runtime/events", s.getOnly(s.requireLocalToken(s.agentScoped(s.handleEvents))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/logs/recent", s.getOnly(s.requireLocalToken(s.agentScoped(s.handleLogsRecent))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/logs/stream", s.getOnly(s.requireLocalToken(s.agentScoped(s.handleLogsStream))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/control/pause", s.postOnly(s.requireLocalToken(s.agentScoped(s.handlePause))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/control/resume", s.postOnly(s.requireLocalToken(s.agentScoped(s.handleResume))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/control/soft-interrupt", s.postOnly(s.requireLocalToken(s.agentScoped(s.handleSoftInterrupt))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/control/intervene", s.postOnly(s.requireLocalToken(s.agentScoped(s.handleIntervene))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/control/handback", s.postOnly(s.requireLocalToken(s.agentScoped(s.handleHandback))))
	mux.HandleFunc("/api/local/connections/{conn}/agents/{name}/control/complete", s.postOnly(s.requireLocalToken(s.agentScoped(s.handleComplete))))

	return s.localCORS(mux)
}

func (s *LocalServer) Start() error {
	if err := ValidateLoopbackBindAddr(s.cfg.BindAddr); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", s.cfg.BindAddr)
	if err != nil {
		return fmt.Errorf("start local control server on %s: %w — another agentd instance may already be running and holding the port", s.cfg.BindAddr, err)
	}
	s.server = &http.Server{Handler: s.Handler()}
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("[local-server] serve failed: %v", err)
		}
	}()
	return nil
}

func (s *LocalServer) getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func (s *LocalServer) postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func (s *LocalServer) deleteOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodDelete)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func (s *LocalServer) getOnlyHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (s *LocalServer) localCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !isLoopbackOrigin(origin) {
				if r.Method == http.MethodOptions {
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", http.MethodGet+", "+http.MethodPost+", "+http.MethodPut+", "+http.MethodDelete)
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-Local-Token, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *LocalServer) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *LocalServer) requireLocalToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ValidateLocalToken(s.cfg.LocalToken, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// requireRegistry resolves the runtime registry, writing a 503 when absent.
func (s *LocalServer) requireRegistry(w http.ResponseWriter) (RuntimeRegistry, bool) {
	if s.cfg.Registry == nil {
		http.Error(w, "agent registry unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return s.cfg.Registry, true
}

// agentScoped resolves {conn}/{name} to a runtime handle, writing 404 for
// unknown (connection, instance) pairs, then dispatches with the handle in
// the request context.
func (s *LocalServer) agentScoped(next func(http.ResponseWriter, *http.Request, RuntimeHandle)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		registry, ok := s.requireRegistry(w)
		if !ok {
			return
		}
		conn := r.PathValue("conn")
		name := r.PathValue("name")
		runtime, ok := registry.Runtime(conn, name)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown agent %q on connection %q", name, conn), http.StatusNotFound)
			return
		}
		next(w, r, runtime)
	}
}

func (s *LocalServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	body := map[string]interface{}{
		"status":  "ok",
		"version": s.cfg.Version,
	}
	if registry, ok := s.requireRegistryNoWrite(); ok {
		body["agents"] = registry.Agents()
	}
	writeLocalJSON(w, body)
}

// requireRegistryNoWrite resolves the registry without writing an error
// response (health stays 200 even without a registry).
func (s *LocalServer) requireRegistryNoWrite() (RuntimeRegistry, bool) {
	return s.cfg.Registry, s.cfg.Registry != nil
}

func (s *LocalServer) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	registry, ok := s.requireRegistry(w)
	if !ok {
		return
	}
	agents := registry.Agents()
	if connection := strings.TrimSpace(r.URL.Query().Get("connection")); connection != "" {
		scoped := make([]LocalAgentInfo, 0, len(agents))
		for _, a := range agents {
			if a.Connection == connection {
				scoped = append(scoped, a)
			}
		}
		agents = scoped
	}
	writeLocalJSON(w, struct {
		Agents []LocalAgentInfo `json:"agents"`
	}{Agents: agents})
}

// handleConnections serves GET (list workspace connections) and POST (add a
// connection: token + optional alias) for the local control page. A
// connection is name + token only — its instances arrive through desired
// delivery.
func (s *LocalServer) handleConnections(w http.ResponseWriter, r *http.Request) {
	registry, ok := s.requireRegistry(w)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeLocalJSON(w, struct {
			Connections []ConnectionInfo `json:"connections"`
		}{Connections: registry.Connections()})
	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := registry.AddConnection(req.Name, req.Token); err != nil {
			writeRegistryError(w, err)
			return
		}
		writeLocalJSON(w, struct {
			Connections []ConnectionInfo `json:"connections"`
		}{Connections: registry.Connections()})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConnectionDelete removes one workspace connection.
func (s *LocalServer) handleConnectionDelete(w http.ResponseWriter, r *http.Request) {
	registry, ok := s.requireRegistry(w)
	if !ok {
		return
	}
	if err := registry.RemoveConnection(r.PathValue("name")); err != nil {
		writeRegistryError(w, err)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

// handleInstanceMemory serves the per-persona memory management for the
// local console: GET reads the unified memory, PUT replaces it (hand edit),
// DELETE clears it. The memory is addressed by persona key — the shared brain
// of every instance carrying that key on this machine.
func (s *LocalServer) handleInstanceMemory(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Memory == nil {
		http.Error(w, "memory management unavailable", http.StatusServiceUnavailable)
		return
	}
	persona := r.PathValue("persona")
	if strings.TrimSpace(persona) == "" {
		http.Error(w, "persona key is required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		content, err := s.cfg.Memory.Read(persona)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeLocalJSON(w, struct {
			Instance string `json:"instance"`
			Content  string `json:"content"`
		}{Instance: persona, Content: content})
	case http.MethodPut:
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.cfg.Memory.WriteRaw(persona, req.Content); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeLocalJSON(w, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := s.cfg.Memory.Clear(persona); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeLocalJSON(w, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodDelete)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeRegistryError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case strings.Contains(err.Error(), "already exists"):
		status = http.StatusConflict
	case strings.Contains(err.Error(), "not found"):
		status = http.StatusNotFound
	case strings.Contains(err.Error(), "td_ prefix"):
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}

func (s *LocalServer) handleSnapshot(w http.ResponseWriter, _ *http.Request, runtime RuntimeHandle) {
	writeLocalJSON(w, runtime.State().Snapshot())
}

func (s *LocalServer) handlePause(w http.ResponseWriter, _ *http.Request, runtime RuntimeHandle) {
	watchers := runtime.Watchers()
	if len(watchers) == 0 {
		http.Error(w, "watcher unavailable (no workspace identity resolved yet)", http.StatusServiceUnavailable)
		return
	}
	for _, watcher := range watchers {
		watcher.Pause()
	}
	runtime.State().SetPaused(true)
	runtime.Hub().PublishSnapshot("agent.paused", runtime.State().Snapshot())
	writeLocalJSON(w, map[string]bool{"paused": true})
}

func (s *LocalServer) handleResume(w http.ResponseWriter, _ *http.Request, runtime RuntimeHandle) {
	watchers := runtime.Watchers()
	if len(watchers) == 0 {
		http.Error(w, "watcher unavailable (no workspace identity resolved yet)", http.StatusServiceUnavailable)
		return
	}
	for _, watcher := range watchers {
		watcher.Resume()
	}
	runtime.State().SetPaused(false)
	runtime.Hub().PublishSnapshot("agent.resumed", runtime.State().Snapshot())
	writeLocalJSON(w, map[string]bool{"paused": false})
}

// requireChat resolves the chat manager, writing a 503 when not configured.
func (s *LocalServer) requireChat(w http.ResponseWriter) (*ChatManager, bool) {
	if s.cfg.Chat == nil {
		http.Error(w, "chat unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return s.cfg.Chat, true
}

type chatSendRequest struct {
	Provider  string `json:"provider"`
	Prompt    string `json:"prompt"`
	WorkDir   string `json:"workdir"`
	SessionID string `json:"session_id"`
}

func (s *LocalServer) handleChatTools(w http.ResponseWriter, _ *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	writeLocalJSON(w, struct {
		Tools []ChatToolInfo `json:"tools"`
	}{Tools: m.Tools()})
}

func (s *LocalServer) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	writeLocalJSON(w, m.Sessions(r.URL.Query().Get("provider")))
}

func (s *LocalServer) handleChatSessionMessages(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	writeLocalJSON(w, m.SessionMessages(r.URL.Query().Get("provider"), r.URL.Query().Get("id")))
}

func (s *LocalServer) handleChatState(w http.ResponseWriter, _ *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	writeLocalJSON(w, m.State())
}

func (s *LocalServer) handleChatSend(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	var req chatSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	run, err := m.Chat(req.Provider, req.Prompt, req.WorkDir, req.SessionID)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrChatBusy):
			status = http.StatusConflict
		case errors.Is(err, ErrChatUnknownProvider),
			errors.Is(err, ErrChatEmptyPrompt),
			errors.Is(err, ErrChatWorkDirInvalid):
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeLocalJSON(w, struct {
		OK     bool           `json:"ok"`
		Active *ChatActiveRun `json:"active"`
	}{OK: true, Active: run})
}

func (s *LocalServer) handleChatStop(w http.ResponseWriter, _ *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	if err := m.Stop(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleChatReset(w http.ResponseWriter, _ *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	m.ResetSession()
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleChatLogsRecent(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	writeLocalJSON(w, struct {
		Lines []LogLine `json:"lines"`
	}{Lines: m.Buffer().Recent(parseLogLimit(r, m.Buffer().Capacity()))})
}

// parseLogLimit reads the optional ?limit= query parameter for the recent-logs
// endpoints. Unset, invalid, or non-positive values fall back to the historical
// default of 500; explicit values are clamped to the buffer capacity.
func parseLogLimit(r *http.Request, capacity int) int {
	const defaultLimit = 500
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if capacity > 0 && n > capacity {
		return capacity
	}
	return n
}

func (s *LocalServer) handleChatLogsStream(w http.ResponseWriter, r *http.Request) {
	m, ok := s.requireChat(w)
	if !ok {
		return
	}
	serveLogSSE(w, r, m.Buffer(), LocalEventChatOutputLine)
}

func (s *LocalServer) handleControlPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(web.ControlIndexHTML())
}

type controlRequest struct {
	TaskID  int32  `json:"task_id"`
	NodeID  string `json:"node_id"`
	Message string `json:"message"`
}

func (s *LocalServer) handleSoftInterrupt(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := runtime.Executor().SoftInterrupt(req.TaskID, req.NodeID); err != nil {
		if errors.Is(err, ErrInterventionNotHeld) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleIntervene(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	out, err := runtime.Executor().ExecuteInterventionTurn(req.TaskID, req.NodeID, req.Message)
	if err != nil {
		switch {
		case errors.Is(err, ErrInterventionNotHeld):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrCodingToolUnavailable):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeLocalJSON(w, map[string]string{"ok": "true", "output": out})
}

func (s *LocalServer) handleHandback(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !runtime.Executor().Handback(req.TaskID, req.NodeID) {
		http.Error(w, "not running", http.StatusConflict)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleComplete(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !runtime.Executor().CompleteManually(req.TaskID, req.NodeID) {
		http.Error(w, "not running", http.StatusConflict)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleLogsRecent(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	writeLocalJSON(w, struct {
		Lines []LogLine `json:"lines"`
	}{Lines: runtime.Buffer().Recent(parseLogLimit(r, runtime.Buffer().Capacity()))})
}

func (s *LocalServer) handleLogsStream(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	serveLogSSE(w, r, runtime.Buffer(), LocalEventOutputLine)
}

// serveLogSSE streams a LogBuffer over SSE: lines after ?since=<seq> are
// replayed, then live lines are forwarded as named events with a 15s
// keepalive ping. It backs both the execution log stream and the direct-chat
// log stream.
func serveLogSSE(w http.ResponseWriter, r *http.Request, lb *LogBuffer, eventName string) {
	if lb == nil {
		http.Error(w, "log streaming unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Replay any lines the client hasn't seen yet, then subscribe for live ones.
	var since uint64
	if v := r.URL.Query().Get("since"); v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = parsed
		}
	}
	for _, line := range lb.Since(since) {
		writeLogSSE(w, line, eventName)
	}
	flusher.Flush()

	events, unsubscribe := lb.Subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-events:
			if !ok {
				return
			}
			writeLogSSE(w, line, eventName)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeLogSSE(w http.ResponseWriter, line LogLine, eventName string) {
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\n", eventName)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func (s *LocalServer) handleEvents(w http.ResponseWriter, r *http.Request, runtime RuntimeHandle) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	events, unsubscribe := runtime.Hub().Subscribe()
	defer unsubscribe()

	s.writeSSE(w, LocalEvent{Type: LocalEventSnapshotUpdated, Snapshot: runtime.State().Snapshot()})
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			s.writeSSE(w, event)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *LocalServer) writeSSE(w http.ResponseWriter, event LocalEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if event.EventID == "" {
		event.EventID = fmt.Sprintf("local-%d", event.Timestamp.UnixMilli())
	}
	if event.InstanceID == "" {
		event.InstanceID = event.Snapshot.InstanceID
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\n", event.Type)
	fmt.Fprintf(w, "id: %s\n", event.EventID)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeLocalJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func ValidateLoopbackBindAddr(bindAddr string) error {
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return fmt.Errorf("parse local control bind addr %q: %w", bindAddr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("local control bind addr must be loopback, got %s", bindAddr)
	}
	return nil
}

func isLoopbackOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
