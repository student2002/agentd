// local_server.go 提供本地模式下的 HTTP 服务端。
package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed local_control_page.html
var localControlPageHTML []byte

type LocalServerConfig struct {
	BindAddr   string
	LocalToken string
	Version    string

	// Executor supplies recent + streaming execution logs. Optional; when nil
	// the logs endpoints return empty / no events.
	Executor *TaskExecutor
	// LogBuffer is the source of execution output lines for the logs endpoints.
	// Optional; when nil the logs endpoints return empty / no events.
	LogBuffer *LogBuffer
	// Watcher is used by the pause/resume control endpoints. Optional; when nil
	// the control endpoints return 503.
	Watcher *NodeWatcher
}

type LocalServer struct {
	cfg    LocalServerConfig
	state  *LocalStateStore
	hub    *LocalEventHub
	server *http.Server
}

func NewLocalServer(cfg LocalServerConfig, state *LocalStateStore, hub *LocalEventHub) *LocalServer {
	if cfg.BindAddr == "" {
		cfg.BindAddr = "127.0.0.1:17380"
	}
	return &LocalServer{
		cfg:   cfg,
		state: state,
		hub:   hub,
	}
}

func (s *LocalServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/local/health", s.getOnly(s.handleHealth))
	mux.HandleFunc("/api/local/runtime/snapshot", s.getOnly(s.requireLocalToken(s.handleSnapshot)))
	mux.HandleFunc("/api/local/events", s.getOnly(s.requireLocalToken(s.handleEvents)))
	mux.HandleFunc("/api/local/logs/recent", s.getOnly(s.requireLocalToken(s.handleLogsRecent)))
	mux.HandleFunc("/api/local/logs/stream", s.getOnly(s.requireLocalToken(s.handleLogsStream)))
	mux.HandleFunc("/api/local/control/pause", s.postOnly(s.requireLocalToken(s.handlePause)))
	mux.HandleFunc("/api/local/control/resume", s.postOnly(s.requireLocalToken(s.handleResume)))
	mux.HandleFunc("/api/local/control/soft-interrupt", s.postOnly(s.requireLocalToken(s.handleSoftInterrupt)))
	mux.HandleFunc("/api/local/control/intervene", s.postOnly(s.requireLocalToken(s.handleIntervene)))
	mux.HandleFunc("/api/local/control/handback", s.postOnly(s.requireLocalToken(s.handleHandback)))
	mux.HandleFunc("/api/local/control/complete", s.postOnly(s.requireLocalToken(s.handleComplete)))
	mux.HandleFunc("/api/local/control/page", s.getOnly(s.handleControlPage))
	return s.localCORS(mux)
}

func (s *LocalServer) Start() error {
	if err := ValidateLoopbackBindAddr(s.cfg.BindAddr); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", s.cfg.BindAddr)
	if err != nil {
		return fmt.Errorf("start local control server on %s: %w", s.cfg.BindAddr, err)
	}
	s.server = &http.Server{Handler: s.Handler()}
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.state.SetRuntimeError("local_server_failed", err.Error())
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
				w.Header().Set("Access-Control-Allow-Methods", http.MethodGet+", "+http.MethodPost)
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

func (s *LocalServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.state.Snapshot()
	writeLocalJSON(w, map[string]string{
		"status":      "ok",
		"instance_id": snapshot.InstanceID,
		"version":     s.cfg.Version,
	})
}

func (s *LocalServer) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	writeLocalJSON(w, s.state.Snapshot())
}

func (s *LocalServer) handlePause(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Watcher == nil {
		http.Error(w, "watcher unavailable", http.StatusServiceUnavailable)
		return
	}
	s.cfg.Watcher.Pause()
	s.state.SetPaused(true)
	s.hub.PublishSnapshot("agent.paused", s.state.Snapshot())
	writeLocalJSON(w, map[string]bool{"paused": true})
}

func (s *LocalServer) handleResume(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Watcher == nil {
		http.Error(w, "watcher unavailable", http.StatusServiceUnavailable)
		return
	}
	s.cfg.Watcher.Resume()
	s.state.SetPaused(false)
	s.hub.PublishSnapshot("agent.resumed", s.state.Snapshot())
	writeLocalJSON(w, map[string]bool{"paused": false})
}

// SetWatcher attaches the watcher after it is created in Run().
func (s *LocalServer) SetWatcher(w *NodeWatcher) {
	s.cfg.Watcher = w
}

// SetExecutor attaches the executor after construction (it is created in
// NewDaemonWithOptions alongside the local server).
func (s *LocalServer) SetExecutor(e *TaskExecutor) {
	s.cfg.Executor = e
}

func (s *LocalServer) handleControlPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(localControlPageHTML)
}

type controlRequest struct {
	TaskID  int32  `json:"task_id"`
	NodeID  string `json:"node_id"`
	Message string `json:"message"`
}

func (s *LocalServer) requireExecutor(w http.ResponseWriter) (*TaskExecutor, bool) {
	if s.cfg.Executor == nil {
		http.Error(w, "executor unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	return s.cfg.Executor, true
}

func (s *LocalServer) handleSoftInterrupt(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.requireExecutor(w)
	if !ok {
		return
	}
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := exec.SoftInterrupt(req.TaskID, req.NodeID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleIntervene(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.requireExecutor(w)
	if !ok {
		return
	}
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	out, err := exec.ExecuteInterventionTurn(req.TaskID, req.NodeID, req.Message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeLocalJSON(w, map[string]string{"ok": "true", "output": out})
}

func (s *LocalServer) handleHandback(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.requireExecutor(w)
	if !ok {
		return
	}
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !exec.Handback(req.TaskID, req.NodeID) {
		http.Error(w, "not running", http.StatusConflict)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleComplete(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.requireExecutor(w)
	if !ok {
		return
	}
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !exec.CompleteManually(req.TaskID, req.NodeID) {
		http.Error(w, "not running", http.StatusConflict)
		return
	}
	writeLocalJSON(w, map[string]bool{"ok": true})
}

func (s *LocalServer) handleLogsRecent(w http.ResponseWriter, r *http.Request) {
	lb := s.cfg.LogBuffer
	if lb == nil {
		writeLocalJSON(w, struct {
			Lines []LogLine `json:"lines"`
		}{Lines: []LogLine{}})
		return
	}
	writeLocalJSON(w, struct {
		Lines []LogLine `json:"lines"`
	}{Lines: lb.Recent(500)})
}

func (s *LocalServer) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	lb := s.cfg.LogBuffer
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
		s.writeLogSSE(w, line)
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
			s.writeLogSSE(w, line)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *LocalServer) writeLogSSE(w http.ResponseWriter, line LogLine) {
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\n", LocalEventOutputLine)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func (s *LocalServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	events, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	s.writeSSE(w, LocalEvent{Type: LocalEventSnapshotUpdated, Snapshot: s.state.Snapshot()})
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
